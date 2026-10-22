//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// Names and values that several tests share.
const (
	testRepo          = "o/r"
	testName          = "cs-1"
	testWorkspace     = "/workspaces/r"
	stateProvisioning = "Provisioning"
	stepPrepare       = "prepare"
	stepDiff          = "diff"
	cmdTrue           = "true"
	cmdLong           = "long"
	deleteCS1         = "DELETE /user/codespaces/cs-1"
	stopCS1           = "POST /user/codespaces/cs-1/stop"
)

// Errors that fake sandboxes inject.
var (
	errBrokenPipe    = errors.New("broken pipe")
	errConnLost      = errors.New("connection lost")
	errTunnelRefused = errors.New("tunnel refused")
)

// fixture is a Sandboxes wired to in-memory fakes.
type fixture struct {
	*sandbox.Sandboxes

	gh   *sandboxtest.GitHub
	dial *sandboxtest.Dialer
}

func newFixture(t *testing.T, reuse bool) *fixture {
	t.Helper()

	fakeGitHub := new(sandboxtest.GitHub)
	dialer := &sandboxtest.Dialer{GitHub: fakeGitHub, Handler: nil, Err: nil, Hook: nil}

	sandboxes, err := sandbox.New(sandbox.Config{
		Token:     "",
		Getenv:    func(string) string { return "" },
		APIClient: fakeGitHub.Client(),
		Dial: func(
			ctx context.Context, name string, admit func(remote.Codespace) error, progress func(string),
		) (sandbox.Conn, error) {
			conn, err := dialer.Dial(ctx, name, admit, progress)
			if err != nil {
				return nil, fmt.Errorf("fake dial: %w", err)
			}

			return conn, nil
		},
		Logger: nil,
		Reuse:  reuse,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		err := sandboxes.Close()
		if err != nil {
			t.Error(err)
		}
	})

	return &fixture{Sandboxes: sandboxes, gh: fakeGitHub, dial: dialer}
}

// mustCreate creates a sandbox from testRepo and returns its name.
func (rig *fixture) mustCreate(ctx context.Context, t *testing.T) string {
	t.Helper()

	res, err := rig.Create(ctx, newCreateOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return res.Name
}

// commandFunc is the part of a fake sandbox that runs the caller's own
// command, as opposed to runspace's prepare and diff steps.
type commandFunc func(ctx context.Context, cmd remote.Cmd) (int, error)

// userCmd answers runspace's own steps like a healthy sandbox and runs
// command for the caller's.
func userCmd(command commandFunc) sandboxtest.Handler {
	return func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if sandboxtest.Step(cmd) == "" {
			return command(ctx, cmd)
		}

		return runspaceStep(ctx, name, cmd)
	}
}

// runspaceStep answers one of runspace's own steps like a healthy sandbox.
func runspaceStep(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
	code, err := sandboxtest.Default(ctx, name, cmd)
	if err != nil {
		return 0, fmt.Errorf("default step: %w", err)
	}

	return code, nil
}

// exitWith is a command that ends at once with code and err.
func exitWith(code int, err error) commandFunc {
	return func(context.Context, remote.Cmd) (int, error) { return code, err }
}

// blockUntilDone is a command that waits for ctx and returns its cause, like
// remote.Conn.Exec.
func blockUntilDone(ctx context.Context, _ remote.Cmd) (int, error) {
	<-ctx.Done()

	return 0, fmt.Errorf("command ended: %w", context.Cause(ctx))
}

// closed reports whether conn has been closed.
func closed(conn *sandboxtest.Conn) bool {
	select {
	case <-conn.Done():
		return true
	default:
		return false
	}
}

// waitFor polls until condition holds, and fails the test if it never does.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

// The builders below spell out every field, so tests set only what matters.

func newCreateOptions(adjust ...func(*sandbox.CreateOptions)) sandbox.CreateOptions {
	opts := sandbox.CreateOptions{
		Repo:         testRepo,
		Branch:       "",
		SHA:          "",
		Machine:      "",
		Devcontainer: "",
		Idle:         "",
		Retention:    "",
	}
	for _, apply := range adjust {
		apply(&opts)
	}

	return opts
}

func newTimeout(timeout string) sandbox.TimeoutOption {
	return sandbox.TimeoutOption{Timeout: timeout}
}

func newCommand(argv ...string) sandbox.CommandOptions {
	return sandbox.CommandOptions{
		TimeoutOption: newTimeout(""),
		Argv:          argv,
		Env:           nil,
		Workdir:       "",
	}
}

func withEnv(cmd sandbox.CommandOptions, env ...string) sandbox.CommandOptions {
	cmd.Env = env

	return cmd
}

func withWorkdir(cmd sandbox.CommandOptions, dir string) sandbox.CommandOptions {
	cmd.Workdir = dir

	return cmd
}

func withTimeout(cmd sandbox.CommandOptions, timeout string) sandbox.CommandOptions {
	cmd.TimeoutOption = newTimeout(timeout)

	return cmd
}

func newExec(name string, cmd sandbox.CommandOptions) sandbox.ExecOptions {
	return sandbox.ExecOptions{CommandOptions: cmd, Name: name}
}

func newRun(cmd sandbox.CommandOptions, keep string) sandbox.RunOptions {
	return sandbox.RunOptions{CreateOptions: newCreateOptions(), CommandOptions: cmd, Keep: keep}
}

func newCopy(src, dst string) sandbox.CopyOptions {
	return sandbox.CopyOptions{TimeoutOption: newTimeout(""), Src: src, Dst: dst, FollowLink: false}
}

func newDiff(name, output string) sandbox.DiffOptions {
	return sandbox.DiffOptions{TimeoutOption: newTimeout(""), Name: name, Output: output}
}

func newDelete(names ...string) sandbox.DeleteOptions {
	return sandbox.DeleteOptions{Names: names, All: false}
}

func deleteAll() sandbox.DeleteOptions {
	return sandbox.DeleteOptions{Names: nil, All: true}
}

// noStreams returns Streams without any member.
func noStreams() sandbox.Streams {
	return sandbox.Streams{Stdin: nil, Stdout: nil, Stderr: nil, TTY: nil, Progress: nil}
}

// streamsTo returns Streams whose only member is stdout.
func streamsTo(stdout io.Writer) sandbox.Streams {
	return sandbox.Streams{Stdin: nil, Stdout: stdout, Stderr: nil, TTY: nil, Progress: nil}
}

// streamsFrom returns Streams whose only member is stdin.
func streamsFrom(stdin io.Reader) sandbox.Streams {
	return sandbox.Streams{Stdin: stdin, Stdout: nil, Stderr: nil, TTY: nil, Progress: nil}
}
