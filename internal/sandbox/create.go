//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
)

// Timing of the availability wait.
const (
	pollEvery       = 5 * time.Second
	availableWithin = 15 * time.Minute
)

var (
	// errWaitTimeout is the cause of the availability wait's own deadline.
	errWaitTimeout = errors.New("availability wait expired")

	errNotAvailable = errors.New("did not become available")
	errExit         = errors.New("exit")
)

// unavailableError says that a codespace reached a state it will not leave
// for Available.
type unavailableError struct {
	name  string
	state string
}

func (e *unavailableError) Error() string {
	return fmt.Sprintf("sandbox %s is %s", e.name, e.state)
}

// BaseRef records the commit a sandbox started from. It lives in the
// sandbox, so runspace keeps no state on the host.
const BaseRef = "refs/runspace/base"

// CreateResult describes a new sandbox.
type CreateResult struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Workspace  string `json:"workspace"`
	Base       string `json:"base"`
}

// Create creates a sandbox and waits until it is ready. A failed or
// cancelled create deletes the codespace it made.
func (s *Sandboxes) Create(ctx context.Context, opts CreateOptions, progress func(string)) (CreateResult, error) {
	err := opts.Validate()
	if err != nil {
		return CreateResult{}, err
	}

	res, sess, err := s.create(ctx, &opts, orNoProgress(progress))
	if err != nil {
		if res.Name != "" {
			err = errors.Join(err, s.deleteDetached(ctx, res.Name))
		}

		return CreateResult{}, err
	}

	s.adopt(res.Name, sess)

	return res, nil
}

// deleteDetached deletes a half-made sandbox even when ctx is cancelled.
func (s *Sandboxes) deleteDetached(ctx context.Context, name string) error {
	err := s.dispose(ctx, name, false)
	if err != nil {
		return fmt.Errorf(
			"also failed to delete the half-made sandbox %s; delete it with runspace delete %s or the delete tool: %w",
			name, name, err)
	}

	return nil
}

// create runs the create sequence and returns the session that prepare
// used. Once the codespace exists, the result's Name is set even when err is
// not nil, so the caller decides between delete and --keep. It never cleans
// up itself.
func (s *Sandboxes) create(
	ctx context.Context, opts *CreateOptions, progress func(string),
) (CreateResult, session, error) {
	progress("Creating a sandbox from " + opts.Repo)
	// The POST runs detached: GitHub may create the codespace even if the
	// caller gives up mid-request, and only a returned name lets the caller
	// delete it.
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	codespace, err := s.api.Create(postCtx, opts.Repo, github.CreateRequest{
		Ref:                        opts.Branch,
		Machine:                    opts.Machine,
		DevcontainerPath:           opts.Devcontainer,
		DisplayName:                DisplayName,
		IdleTimeoutMinutes:         int(opts.idle / time.Minute),
		RetentionPeriodMinutes:     int(opts.retention / time.Minute),
		MultiRepoPermissionsOptOut: true,
	})
	if err != nil {
		return CreateResult{}, session{}, fmt.Errorf("%w", err)
	}

	s.logger.Debug("Created the codespace", "name", codespace.Name, "state", codespace.State)

	res := CreateResult{
		Name:       codespace.Name,
		Repository: codespace.Repository.FullName,
		Workspace:  workspacePath(codespace.Repository.Name),
		Base:       "",
	}

	if ctx.Err() != nil {
		return res, session{}, fmt.Errorf("%w", context.Cause(ctx))
	}

	err = s.waitAvailable(ctx, codespace, progress)
	if err != nil {
		return res, session{}, err
	}

	progress("Connecting to " + codespace.Name)

	// No admit: create made this codespace, and deletes it on failure.
	conn, err := s.dial(ctx, codespace.Name, nil, progress)
	if err != nil {
		return res, session{}, err
	}

	res.Base, err = prepare(ctx, conn, res.Workspace, opts.SHA)
	if err != nil {
		_ = conn.Close()

		return res, session{}, err
	}

	return res, session{conn: conn, workspace: res.Workspace}, nil
}

// waitAvailable polls the codespace until it is Available, fails, or the
// wait's own deadline passes.
func (s *Sandboxes) waitAvailable(ctx context.Context, codespace *github.Codespace, progress func(string)) error {
	ctx, cancel := context.WithTimeoutCause(ctx, availableWithin, errWaitTimeout)
	defer cancel()

	timer := time.NewTimer(pollEvery)
	defer timer.Stop()

	last := ""

	for {
		switch codespace.State {
		case "Available":
			return nil
		case "Failed", "Deleted", "Archived":
			return &unavailableError{name: codespace.Name, state: codespace.State}
		}

		if codespace.State != last {
			progress(fmt.Sprintf("Waiting for %s to become available (%s)", codespace.Name, codespace.State))
			last = codespace.State
		}

		timer.Reset(pollEvery)

		select {
		case <-ctx.Done():
			return waitError(ctx, codespace.Name, last)
		case <-timer.C:
		}

		next, err := s.api.Get(ctx, codespace.Name)
		if err != nil {
			if ctx.Err() != nil {
				return waitError(ctx, codespace.Name, last)
			}

			return fmt.Errorf("%w", err)
		}

		codespace = next
	}
}

func waitError(ctx context.Context, name, state string) error {
	if errors.Is(context.Cause(ctx), errWaitTimeout) {
		return fmt.Errorf("sandbox %s %w within %s; its last state was %s",
			name, errNotAvailable, FormatDuration(availableWithin), state)
	}

	return fmt.Errorf("%w", context.Cause(ctx))
}

// prepareScript checks readiness and records the base: HEAD must resolve,
// equal $1 when $1 is set, and is then stored as refs/runspace/base.
const prepareScript = `set -e
head=$(git rev-parse --verify HEAD)
if [ -n "$1" ] && [ "$head" != "$1" ]; then
	echo "HEAD is $head, not $1" >&2
	exit 1
fi
git update-ref ` + BaseRef + ` "$head"
echo "$head"`

// prepare runs prepareScript in the workspace and returns the base commit.
func prepare(ctx context.Context, conn Conn, workspace, sha string) (string, error) {
	stdout, stderr := new(capped), new(capped)

	code, err := conn.Exec(ctx, remote.Cmd{
		Dir:    workspace,
		Env:    nil,
		Argv:   scriptArgv(prepareScript, sha),
		Stdin:  nil,
		Stdout: stdout,
		Stderr: stderr,
		TTY:    nil,
	})
	if err != nil {
		return "", fmt.Errorf("prepare sandbox: %w", err)
	}

	if code != 0 {
		return "", fmt.Errorf("prepare sandbox: %w", exited(errExit, code, stderr))
	}

	return strings.TrimSpace(stdout.String()), nil
}
