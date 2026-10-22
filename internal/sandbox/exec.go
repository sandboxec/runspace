//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"go.sandbox.ec/runspace/internal/remote"
)

var errInterrupted = errors.New("interrupted")

// ExitError is the outcome of exec or run whenever it is not a plain
// success. Code is one of:
//   - the remote exit code, including 126, 127, and 128+n;
//   - 124 when the timeout expired;
//   - 130 when the call was interrupted;
//   - 125 for any runspace failure.
//
// Err is nil for a plain non-zero remote exit, which prints nothing.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("remote command exited %d", e.Code)
	}

	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Exit codes for runspace's own outcomes.
const (
	ExitTimeout     = 124
	ExitFailure     = 125
	ExitInterrupted = 130
)

// exitStatus applies the exit-code rule to the end of exec or run. ctx is
// the caller's context; code and err come from the remote command, or err
// from an earlier step.
func exitStatus(ctx context.Context, code int, err error) error {
	switch {
	case err == nil && code == 0:
		return nil
	case err == nil:
		return &ExitError{Code: code, Err: nil}
	case errors.Is(err, errTimeout):
		return &ExitError{Code: ExitTimeout, Err: err}
	case ctx.Err() != nil:
		return &ExitError{Code: ExitInterrupted, Err: errInterrupted}
	default:
		return &ExitError{Code: ExitFailure, Err: err}
	}
}

// ExitCode returns the process exit code of an Exec or Run result.
func ExitCode(err error) int {
	var exitErr *ExitError

	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.Code
	default:
		return ExitFailure
	}
}

// Exec runs one command in a sandbox. Every non-nil error is an *ExitError.
func (s *Sandboxes) Exec(ctx context.Context, opts ExecOptions, streams Streams) error {
	err := opts.Validate()
	if err != nil {
		return &ExitError{Code: ExitFailure, Err: err}
	}

	held, err := s.acquire(ctx, opts.Name, streams.progress())
	if err != nil {
		return exitStatus(ctx, 0, err)
	}

	return s.command(ctx, opts.Name, held, &opts.CommandOptions, streams)
}

// command runs opts in sandbox name over held, releases it, and applies the
// exit-code rule.
func (s *Sandboxes) command(
	ctx context.Context, name string, held *lease, opts *CommandOptions, streams Streams,
) error {
	dir := workdir(held.workspace, opts.Workdir)
	// Not the command line or the environment, which can hold secrets.
	s.logger.Debug("Running the command", "name", name, "dir", dir, "tty", streams.TTY != nil)

	start := time.Now()

	code, err := held.run(ctx, &opts.TimeoutOption, "command", remote.Cmd{
		Dir:    dir,
		Env:    opts.Env,
		Argv:   opts.Argv,
		Stdin:  streams.Stdin,
		Stdout: streams.Stdout,
		Stderr: streams.Stderr,
		TTY:    streams.TTY,
	})
	s.logger.Debug("The command ended", "name", name, "code", code, "took", time.Since(start).Round(time.Millisecond))

	return exitStatus(ctx, code, err)
}

// workdir resolves a path given by the caller, such as -w or the sandbox side
// of cp, against the workspace: a relative path is joined to it.
func workdir(workspace, dir string) string {
	if path.IsAbs(dir) {
		return path.Clean(dir)
	}

	return path.Join(workspace, dir)
}

// RunResult says what run did with its sandbox. Each front end words the
// notices it implies.
type RunResult struct {
	Name string `json:"name"`
	Kept bool   `json:"kept"`
	// Retention is how long a kept sandbox survives once stopped.
	Retention time.Duration `json:"-"`
	// CleanupErr is a failed stop or delete. After a failed delete the
	// sandbox is still there; after a failed stop its idle timeout stops it.
	CleanupErr error `json:"-"`
}

// Run creates a sandbox, runs one command in it, and then deletes it, or
// stops it when opts.Keep says so. A readiness failure counts as a failed
// command. Every non-nil error is an *ExitError.
func (s *Sandboxes) Run(ctx context.Context, opts RunOptions, streams Streams) (RunResult, error) {
	err := opts.Validate()
	if err != nil {
		return RunResult{}, &ExitError{Code: ExitFailure, Err: err}
	}

	res, sess, err := s.create(ctx, &opts.CreateOptions, streams.progress())
	if res.Name == "" {
		return RunResult{}, exitStatus(ctx, 0, err)
	}

	var outcome error
	if err != nil {
		outcome = exitStatus(ctx, 0, err)
	} else {
		outcome = s.command(ctx, res.Name, sess.closing(), &opts.CommandOptions, streams)
	}

	kept := keep(opts.Keep, ExitCode(outcome))

	return RunResult{
		Name:       res.Name,
		Kept:       kept,
		Retention:  opts.retention,
		CleanupErr: s.dispose(ctx, res.Name, kept),
	}, outcome
}

// keep applies --keep to a final exit code.
func keep(policy string, code int) bool {
	return policy == KeepAlways || policy == KeepFailure && code != 0
}

// dispose stops a kept sandbox or deletes any other, on a context detached
// from cancellation. A failed stop never falls back to delete: the idle
// timeout stops the sandbox anyway.
func (s *Sandboxes) dispose(ctx context.Context, name string, kept bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	s.logger.Debug("Cleaning up", "name", name, "stop", kept)

	if kept {
		err := s.api.Stop(ctx, name)
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		return nil
	}

	err := s.api.Delete(ctx, name)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
