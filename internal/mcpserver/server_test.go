//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sandbox.ec/runspace/internal/mcpserver"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// Scripted commands of the result tests, named by their argv[0].
const (
	cmdBig    = "big"
	cmdStdin  = "stdin"
	cmdSleep  = "sleep"
	cmdBroken = "broken"
	cmdOwn125 = "own125"
)

// validated is the options struct of a tool, which the CLI validates too.
type validated interface{ Validate() error }

// cliMessage returns the error message the CLI reports for args, which
// describe options, or "" when the CLI accepts them.
func cliMessage(t *testing.T, args map[string]any, options validated) string {
	t.Helper()

	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(encoded, options)
	if err != nil {
		t.Fatal(err)
	}

	err = options.Validate()
	if err == nil {
		return ""
	}

	return err.Error()
}

// Each tool rejects the same invalid inputs as its CLI
// command, with the same message.
func TestSameValidation(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, nil)

	cases := []struct {
		tool    string
		args    map[string]any
		options validated
	}{
		{toolExec, with(execArgs(testName, "x"), "env", []string{"1A=x"}), new(sandbox.ExecOptions)},
		{toolExec, with(execArgs(testName, "x"), "env", []string{"HOME"}), new(sandbox.ExecOptions)},
		{toolRun, map[string]any{keyRepo: testRepo, keyArgv: []string{"x"}, "env": []string{"HOME"}},
			new(sandbox.RunOptions)},
		{toolExec, execArgs(testName), new(sandbox.ExecOptions)},
		{toolRun, map[string]any{keyRepo: testRepo, keyArgv: []string{"x"}, keyKeep: "sometimes"}, new(sandbox.RunOptions)},
		{toolCreate, map[string]any{keyRepo: testRepo, "retention": "30s"}, new(sandbox.CreateOptions)},
		{toolCp, map[string]any{keySrc: "cs-1:a", keyDst: "cs-2:b"}, new(sandbox.CopyOptions)},
		{toolCp, map[string]any{keySrc: "-", keyDst: "-"}, new(sandbox.CopyOptions)},
		{toolCp, map[string]any{keySrc: "-", keyDst: "local"}, new(sandbox.CopyOptions)},
		{toolDelete, map[string]any{"names": []string{testName}, "all": true}, new(sandbox.DeleteOptions)},
		{toolDelete, map[string]any{}, new(sandbox.DeleteOptions)},
		{toolDiff, map[string]any{keyName: "../x"}, new(sandbox.DiffOptions)},
	}
	for _, test := range cases {
		want := cliMessage(t, test.args, test.options)
		if want == "" {
			t.Fatalf("%s %v: CLI accepts it", test.tool, test.args)
		}

		res := fix.call(t, test.tool, test.args)
		if !res.IsError || text(res) != want {
			t.Errorf("%s %v: error %v %q, want %q", test.tool, test.args, res.IsError, text(res), want)
		}
	}

	if creates := fix.github.Creates(); len(creates) != 0 {
		t.Errorf("rejected calls created %d codespaces", len(creates))
	}
}

func TestRejectsInputsWithoutCLIEquivalent(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, nil)

	if res := fix.call(t, toolCp, map[string]any{keySrc: "-", keyDst: "cs-1:d"}); !res.IsError {
		t.Error(`cp accepted "-"`)
	}

	if res := fix.call(t, toolExec, with(execArgs(testName, "x"), "tty", true)); !res.IsError {
		t.Error("exec accepted an unknown field")
	}
}

// scriptedCommands runs the commands the result tests name by argv[0].
func scriptedCommands(t *testing.T) sandboxtest.Handler {
	t.Helper()

	return userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		switch cmd.Argv[0] {
		case cmdBig:
			reply(t, cmd.Stdout, "HEAD"+strings.Repeat("x", mcpserver.OutputCap)+"TAIL")
			reply(t, cmd.Stderr, "err")

			return 3, nil
		case cmdStdin:
			input, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				t.Error(err)
			}

			reply(t, cmd.Stdout, string(input))
		case cmdSleep:
			return blockUntilCancelled(ctx, cmd)
		case cmdBroken:
			return 0, errConnectionLost
		case cmdOwn125:
			reply(t, cmd.Stdout, "skip")

			return 125, nil
		}

		return 0, nil
	})
}

// Output is capped to its tail, with a marker.
func TestOutputCap(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	out := decodeCommand(t, fix.call(t, toolExec, execArgs(fix.create(t), cmdBig)))

	if out.ExitCode != 3 || out.Stderr != "err" || !out.Truncated {
		t.Errorf("big: %+v", out)
	}

	hasMarker := strings.HasPrefix(out.Stdout, "[truncated 8 bytes]\n")
	if !hasMarker || !strings.HasSuffix(out.Stdout, "TAIL") || len(out.Stdout) > mcpserver.OutputCap+32 {
		t.Errorf("big stdout: %d bytes, starts %q", len(out.Stdout), out.Stdout[:30])
	}
}

func TestStdin(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	args := with(execArgs(fix.create(t), cmdStdin), "stdin", "hello")
	out := decodeCommand(t, fix.call(t, toolExec, args))

	if out.Stdout != "hello" || out.Truncated {
		t.Errorf("stdin: %+v", out)
	}
}

// 124 is a result, with runspace's note on stderr.
func TestTimeoutIsAResult(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	res := fix.call(t, toolExec, with(execArgs(fix.create(t), cmdSleep), "timeout", "20ms"))
	out := decodeCommand(t, res)

	if res.IsError || out.ExitCode != 124 || !strings.Contains(out.Stderr, "timeout") {
		t.Errorf("timeout: error=%v %+v", res.IsError, out)
	}
}

// 125 with a cause is runspace's failure, a tool error.
func TestRunspaceFailureIsAnError(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	res := fix.call(t, toolExec, execArgs(fix.create(t), cmdBroken))

	if !res.IsError || !strings.Contains(text(res), "connection lost") {
		t.Errorf("runspace failure: error=%v %q", res.IsError, text(res))
	}
}

// A command's own 125 is a result.
func TestOwn125IsAResult(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	res := fix.call(t, toolExec, execArgs(fix.create(t), cmdOwn125))

	if out := decodeCommand(t, res); res.IsError || out.ExitCode != 125 || out.Stdout != "skip" {
		t.Errorf("a command's own 125: error=%v %+v", res.IsError, out)
	}
}

func TestDiffReturnsPatch(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	res := fix.call(t, toolDiff, map[string]any{keyName: fix.create(t)})

	if res.IsError || text(res) != sandboxtest.Patch {
		t.Errorf("diff: %q", text(res))
	}
}

// The run tool reports its kept sandbox, and ls and delete see it.
func TestRunKeepLsDelete(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, scriptedCommands(t))
	fix.create(t)

	args := map[string]any{keyRepo: testRepo, keyArgv: []string{cmdBig}, keyKeep: "failure"}
	run := decodeRun(t, fix.call(t, toolRun, args))

	if run.ExitCode != 3 || !run.Kept || run.Name == "" {
		t.Errorf("run: %+v", run)
	}

	if !strings.Contains(run.Stderr, "is kept, stopped") || strings.Contains(run.Stderr, "exec -t") {
		t.Errorf("run notice: %q", run.Stderr)
	}

	var listed mcpserver.LsOutput

	decode(t, fix.call(t, toolLs, map[string]any{}), &listed)

	if len(listed.Sandboxes) != 2 {
		t.Errorf("ls: %+v", listed)
	}

	var deleted mcpserver.DeleteOutput

	decode(t, fix.call(t, toolDelete, map[string]any{"all": true}), &deleted)

	if len(deleted.Deleted) != 2 {
		t.Errorf("delete: %+v", deleted)
	}
}

func TestOversizedDiff(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if sandboxtest.Step(cmd) == stepDiff {
			reply(t, cmd.Stdout, strings.Repeat("+", mcpserver.OutputCap+1))

			return 0, nil
		}

		return sandboxtest.Default(ctx, name, cmd)
	})

	res := fix.call(t, toolDiff, map[string]any{keyName: fix.create(t)})
	if !res.IsError || !strings.Contains(text(res), "set output") {
		t.Errorf("oversized diff: error=%v %q", res.IsError, text(res))
	}
}

// Relative host paths resolve against the working directory.
func TestRelativeHostPaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	fix := newFixture(t, nil)
	res := fix.call(t, toolDiff, map[string]any{keyName: fix.create(t), "output": "task.patch"})

	if res.IsError {
		t.Fatal(text(res))
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	patch, err := root.ReadFile("task.patch")
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "task.patch")
	if string(patch) != sandboxtest.Patch || !strings.Contains(text(res), want) {
		t.Errorf("patch at %s: %q; result %q", want, patch, text(res))
	}
}

// A run that fails names its sandbox, so the agent can delete a kept one.
func TestRunFailureNamesSandbox(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, func(context.Context, string, remote.Cmd) (int, error) { return 1, nil })
	args := map[string]any{keyRepo: testRepo, keyArgv: []string{"x"}, keyKeep: "always"}

	res := fix.call(t, toolRun, args)
	if !res.IsError || !strings.Contains(text(res), "sandbox cs-1 is kept, stopped") {
		t.Errorf("error=%v %q", res.IsError, text(res))
	}
}

// A run whose delete fails says so, so the agent can delete the sandbox.
func TestRunCleanupFailure(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, nil)
	fix.github.Fail = map[string]int{"DELETE /user/codespaces/cs-1": 500}

	res := fix.call(t, toolRun, map[string]any{keyRepo: testRepo, keyArgv: []string{"true"}})
	out := decodeRun(t, res)

	if res.IsError || out.Kept || !strings.Contains(out.Stderr, "sandbox cs-1 could not be deleted") {
		t.Errorf("error=%v %+v", res.IsError, out)
	}
}

// Each tool call is logged at DebugLevel with its tool and how it ended. The
// log never holds exec's command line or environment, not even through an
// error that quotes them.
func TestToolCallsAreLogged(t *testing.T) {
	t.Parallel()

	const (
		envValue = "env-sentinel"
		argument = "argv-sentinel"
	)

	fix := newFixture(t, nil)
	name := fix.create(t)
	fix.call(t, toolExec, with(execArgs(name, "echo", argument), "env", []string{"SECRET=" + envValue}))

	// With env set, a command that looks like an assignment is refused, and
	// the error quotes it.
	misused := with(execArgs(name, "API_KEY="+argument), "env", []string{"SECRET=" + envValue})
	if res := callWith(t, fix.client, toolCall(toolExec, misused)); !res.IsError {
		t.Fatalf("exec of an assignment succeeded: %s", text(res))
	}

	logs := fix.logs.String()
	for _, want := range []string{
		"The tool call ended tool=create failed=false", "The tool call ended tool=exec failed=false",
		"The tool call ended tool=exec failed=true", "Running the command", "Reusing the connection",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %q: %s", want, logs)
		}
	}

	for _, secret := range []string{envValue, argument} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q: %s", secret, logs)
		}
	}
}
