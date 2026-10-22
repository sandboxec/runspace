//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// realShell runs each command through a local sh exactly as sshd hands the
// command line to the login shell.
func realShell(ctx context.Context, cmd remote.Cmd) (int, error) {
	//nolint:gosec // G204: running the command line the code under test built, through sh, is the point of the test.
	local := exec.CommandContext(ctx, "sh", "-c", remote.CommandLine(cmd.Dir, cmd.Env, cmd.Argv))
	local.Stdin, local.Stdout, local.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr

	err := local.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}

	if err != nil {
		return 0, fmt.Errorf("run sh: %w", err)
	}

	return 0, nil
}

// shell runs commands in a sandbox backed by a real sh, in a scratch
// directory.
type shell struct {
	rig  *fixture
	name string
	dir  string
}

func newShell(t *testing.T) *shell {
	t.Helper()

	rig := newFixture(t, false)
	rig.dial.Handler = userCmd(realShell)

	return &shell{rig: rig, name: rig.mustCreate(context.Background(), t), dir: t.TempDir()}
}

// run returns the combined output and exit code of opts, with stdin when it
// is not empty.
func (s *shell) run(opts sandbox.CommandOptions, stdin string) (string, int) {
	var out bytes.Buffer

	streams := sandbox.Streams{Stdin: nil, Stdout: &out, Stderr: &out, TTY: nil, Progress: nil}
	if stdin != "" {
		streams.Stdin = strings.NewReader(stdin)
	}

	if opts.Workdir == "" {
		opts.Workdir = s.dir
	}

	err := s.rig.Exec(context.Background(), newExec(s.name, opts), streams)

	return out.String(), sandbox.ExitCode(err)
}

type shellCase struct {
	name        string
	cmd         sandbox.CommandOptions
	stdin       string
	checkOutput bool
	wantOutput  string
	wantCode    int
}

func shellCases(workdir string) []shellCase {
	words := []string{"a b", "it's", "$HOME", "`id`", "line1\nline2", "", "-n", `back\slash`, "*"}

	var printed strings.Builder
	for _, word := range words {
		fmt.Fprintf(&printed, "[%s]\n", word)
	}

	printEnv := withEnv(newCommand("sh", "-c", `printf '%s|%s' "$A" "${EMPTY-unset}"`), "A=x 'y' $HOME\nz", "EMPTY=")

	return []shellCase{
		{"argv words", newCommand(append([]string{"printf", "[%s]\\n"}, words...)...), "", true, printed.String(), 0},
		{"env", printEnv, "", true, "x 'y' $HOME\nz|", 0},
		{"stdin", newCommand("cat"), "piped", true, "piped", 0},
		{"no stdin", newCommand("cat"), "", true, "", 0},
		{"missing workdir", withWorkdir(newCommand("pwd"), workdir+"/missing"), "", false, "", sandbox.ExitFailure},
		{"exit 7", newCommand("sh", "-c", "exit 7"), "", false, "", 7},
		{"killed", newCommand("sh", "-c", "kill -9 $$"), "", false, "", 137},
		{"missing command", newCommand("no-such-command-runspace"), "", false, "", 127},
	}
}

// Quoting round-trips through a real sh, and the wrapper supplies runspace's
// exit codes.
func TestQuotingThroughRealShell(t *testing.T) {
	t.Parallel()

	local := newShell(t)

	for _, test := range shellCases(local.dir) {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			out, code := local.run(test.cmd, test.stdin)
			if code != test.wantCode || test.checkOutput && out != test.wantOutput {
				t.Errorf("exit %d, output %q; want exit %d, output %q (checked: %v)",
					code, out, test.wantCode, test.wantOutput, test.checkOutput)
			}
		})
	}
}

// Relative workdirs join the workspace; env entries pass as given.
func TestCommandResolution(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	name := rig.mustCreate(context.Background(), t)
	cmd := withWorkdir(withEnv(newCommand(cmdTrue), "SET=1", "EMPTY="), "sub/dir")

	err := rig.Exec(context.Background(), newExec(name, cmd), noStreams())
	if err != nil {
		t.Fatal(err)
	}

	cmds := rig.dial.Conns()[1].Cmds()

	got := cmds[len(cmds)-1]
	if got.Dir != testWorkspace+"/sub/dir" {
		t.Errorf("Dir = %q", got.Dir)
	}

	if want := "SET=1 EMPTY="; strings.Join(got.Env, " ") != want {
		t.Errorf("Env = %q, want %q", got.Env, want)
	}
}

type exitCase struct {
	name    string
	command commandFunc
	timeout string
	want    int
}

func exitCases() []exitCase {
	return []exitCase{
		{"success", exitWith(0, nil), "", 0},
		{"remote 42", exitWith(42, nil), "", 42},
		{"transport error", exitWith(0, errBrokenPipe), "", sandbox.ExitFailure},
		{"timeout", blockUntilDone, "1m", sandbox.ExitTimeout},
	}
}

// The exit-code rule.
func TestExitCodeRule(t *testing.T) {
	t.Parallel()

	for _, test := range exitCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rig := newFixture(t, false)
				rig.dial.Handler = userCmd(test.command)
				name := rig.mustCreate(context.Background(), t)

				err := rig.Exec(context.Background(), newExec(name, withTimeout(newCommand("x"), test.timeout)), noStreams())
				if got := sandbox.ExitCode(err); got != test.want {
					t.Errorf("exit %d, want %d (%v)", got, test.want, err)
				}
			})
		})
	}
}

// A command that runs out of time leaves a healthy Conn in the cache: the
// next call reuses it.
func TestExecTimeoutKeepsCachedConn(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newFixture(t, true)
		rig.dial.Handler = userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
			if cmd.Argv[0] == "stall" {
				return blockUntilDone(ctx, cmd)
			}

			return 0, nil
		})
		name := rig.mustCreate(context.Background(), t)

		err := rig.Exec(context.Background(), newExec(name, withTimeout(newCommand("stall"), "1m")), noStreams())
		if got := sandbox.ExitCode(err); got != sandbox.ExitTimeout {
			t.Fatalf("exit %d, want %d (%v)", got, sandbox.ExitTimeout, err)
		}

		if err == nil || !strings.Contains(err.Error(), "command did not finish within the 1m timeout") {
			t.Errorf("err = %v", err)
		}

		err = rig.Exec(context.Background(), newExec(name, newCommand("ok")), noStreams())
		if err != nil {
			t.Fatal(err)
		}

		if n := rig.dial.Dials(name); n != 1 {
			t.Errorf("dials = %d; a timeout discarded the Conn", n)
		}
	})
}

func TestExitCodeInterrupted(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	rig.dial.Handler = userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		cancel()

		return blockUntilDone(ctx, cmd)
	})
	name := rig.mustCreate(context.Background(), t)

	err := rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	if got := sandbox.ExitCode(err); got != sandbox.ExitInterrupted {
		t.Errorf("cancelled: %d", got)
	}
}

func TestExitCodeUsageError(t *testing.T) {
	t.Parallel()

	opts := newExec(testName, newCommand())

	err := opts.Validate()
	if sandbox.ExitCode(&sandbox.ExitError{Code: sandbox.ExitFailure, Err: err}) != sandbox.ExitFailure {
		t.Error("usage error is not 125")
	}
}

// The availability wait's own deadline is a failure, not a timeout.
func TestExitCodeAvailabilityDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newFixture(t, false)
		rig.gh.States = []string{stateProvisioning}

		_, err := rig.Run(context.Background(), newRun(newCommand("x"), ""), noStreams())
		if sandbox.ExitCode(err) != sandbox.ExitFailure || !strings.Contains(err.Error(), "did not become available") {
			t.Errorf("availability deadline: %d, %v", sandbox.ExitCode(err), err)
		}
	})
}

// keepOutcomes maps the exit code a run ends with to the fake command that
// produces it. cancel interrupts the run.
func keepOutcomes() map[int]func(cancel context.CancelFunc) sandboxtest.Handler {
	return map[int]func(cancel context.CancelFunc) sandboxtest.Handler{
		0:   func(context.CancelFunc) sandboxtest.Handler { return userCmd(exitWith(0, nil)) },
		3:   func(context.CancelFunc) sandboxtest.Handler { return userCmd(exitWith(3, nil)) },
		124: func(context.CancelFunc) sandboxtest.Handler { return userCmd(blockUntilDone) },
		125: func(context.CancelFunc) sandboxtest.Handler { return userCmd(exitWith(0, errBrokenPipe)) },
		130: func(cancel context.CancelFunc) sandboxtest.Handler {
			return userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
				cancel()

				return blockUntilDone(ctx, cmd)
			})
		},
	}
}

// The keep matrix. A kept sandbox is stopped, any other is
// deleted, and a failed stop never falls back to delete.
func TestKeepMatrix(t *testing.T) {
	t.Parallel()

	for _, policy := range []string{sandbox.KeepNever, sandbox.KeepFailure, sandbox.KeepAlways} {
		for code, handler := range keepOutcomes() {
			t.Run(fmt.Sprintf("%s/%d", policy, code), func(t *testing.T) {
				t.Parallel()

				synctest.Test(t, func(t *testing.T) {
					checkKeep(t, policy, code, handler)
				})
			})
		}
	}
}

// checkKeep runs under policy a command that ends with code, and checks what
// became of the sandbox.
func checkKeep(t *testing.T, policy string, code int, handler func(cancel context.CancelFunc) sandboxtest.Handler) {
	t.Helper()

	rig := newFixture(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rig.dial.Handler = handler(cancel)

	res, err := rig.Run(ctx, newRun(withTimeout(newCommand("x"), "1m"), policy), noStreams())
	if got := sandbox.ExitCode(err); got != code {
		t.Errorf("%s/%d: exit %d", policy, code, got)
	}

	checkDisposal(t, rig, res, policy, code)
}

// checkDisposal checks that the sandbox of a run that ended with code was
// stopped or deleted as policy says.
func checkDisposal(t *testing.T, rig *fixture, res sandbox.RunResult, policy string, code int) {
	t.Helper()

	wantKeep := policy == sandbox.KeepAlways || policy == sandbox.KeepFailure && code != 0
	stopped := rig.gh.Called(stopCS1)
	deleted := rig.gh.Called(deleteCS1)

	if res.Kept != wantKeep || stopped != wantKeep || deleted == wantKeep {
		t.Errorf("%s/%d: kept=%v stopped=%v deleted=%v, want kept=%v", policy, code, res.Kept, stopped, deleted, wantKeep)
	}

	if res.CleanupErr != nil || res.Name != testName || wantKeep && res.Retention != sandbox.DefaultRetention {
		t.Errorf("%s/%d: result %+v", policy, code, res)
	}
}

func TestKeepFailedStop(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	rig.gh.Fail = map[string]int{stopCS1: 500}
	rig.dial.Handler = userCmd(exitWith(3, nil))

	res, _ := rig.Run(context.Background(), newRun(newCommand("x"), sandbox.KeepFailure), noStreams())

	if rig.gh.Called(deleteCS1) {
		t.Error("a failed stop fell back to delete")
	}

	if !res.Kept || res.CleanupErr == nil {
		t.Errorf("kept=%v cleanup error=%v", res.Kept, res.CleanupErr)
	}
}

func TestKeepFailedDelete(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	rig.gh.Fail = map[string]int{deleteCS1: 500}

	res, err := rig.Run(context.Background(), newRun(newCommand("x"), ""), noStreams())
	if err != nil || res.Kept || res.CleanupErr == nil {
		t.Errorf("err=%v kept=%v cleanup error=%v", err, res.Kept, res.CleanupErr)
	}
}

func TestKeepReadinessFailure(t *testing.T) {
	t.Parallel()

	for _, policy := range []string{sandbox.KeepNever, sandbox.KeepFailure} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, false)
			rig.dial.Handler = func(context.Context, string, remote.Cmd) (int, error) { return 1, nil }

			res, err := rig.Run(context.Background(), newRun(newCommand("x"), policy), noStreams())
			if sandbox.ExitCode(err) != sandbox.ExitFailure {
				t.Errorf("%s: exit %d", policy, sandbox.ExitCode(err))
			}

			if wantKeep := policy == sandbox.KeepFailure; res.Kept != wantKeep || rig.gh.Called(stopCS1) != wantKeep {
				t.Errorf("%s: kept=%v", policy, res.Kept)
			}
		})
	}
}

// A transport error is reported and never retried; with
// reuse, the next call dials again.
func TestTransportErrorNotRetried(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, true)

	var calls atomic.Int32

	rig.dial.Handler = userCmd(func(context.Context, remote.Cmd) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errConnLost
		}

		return 0, nil
	})
	name := rig.mustCreate(ctx, t)

	err := rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	if sandbox.ExitCode(err) != sandbox.ExitFailure || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("err = %v", err)
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("command ran %d times", n)
	}

	err = rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	if err != nil {
		t.Fatal(err)
	}
	// One Conn from create, one after the transport error.
	if n := rig.dial.Dials(name); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}
