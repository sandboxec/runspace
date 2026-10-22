//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/log/v2"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// The command names, and the sandboxes every harness starts with.
const (
	cmdCreate = "create"
	cmdCopy   = "cp"
	cmdDelete = "delete"
	cmdDiff   = "diff"
	cmdExec   = "exec"
	cmdList   = "ls"
	cmdRun    = "run"

	// ownedSandbox was created by runspace; foreignSandbox was not.
	ownedSandbox   = "cs-1"
	foreignSandbox = "mine-1"

	repoFlag = "-R"
	repoName = "o/r"

	msgUnknownCommand = "unknown command"
	msgNotOurs        = "not created by runspace"
)

// harness runs the CLI against in-memory fakes. Sandbox cs-1 exists and is
// tagged.
type harness struct {
	app    *app
	gh     *sandboxtest.GitHub
	dial   *sandboxtest.Dialer
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	// env is the environment that the app reads its log settings from.
	env map[string]string
	// tokens records the token of each newSandboxes call.
	tokens []string
}

func newHarness(t *testing.T, handler sandboxtest.Handler) *harness {
	t.Helper()

	fakeGitHub := newFakeGitHub()
	dialer := &sandboxtest.Dialer{Handler: handler, Err: nil, Hook: nil}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)

	rig := &harness{
		app: &app{
			stdin:        strings.NewReader("input"),
			stdout:       stdout,
			stderr:       stderr,
			tty:          nil,
			log:          newLogger(stderr),
			getenv:       nil,
			verbose:      false,
			dir:          t.TempDir(), // no .git here, so no repository is inferred
			token:        "",
			newSandboxes: nil,
		},
		gh:     fakeGitHub,
		dial:   dialer,
		stdout: stdout,
		stderr: stderr,
		env:    map[string]string{},
		tokens: nil,
	}
	rig.app.getenv = func(key string) string { return rig.env[key] }
	rig.app.newSandboxes = func(token string, reuse bool) (*sandbox.Sandboxes, error) {
		rig.tokens = append(rig.tokens, token)

		return newFakeSandboxes(fakeGitHub, dialer, rig.app.log, reuse)
	}

	return rig
}

// newFakeGitHub returns a fake GitHub holding sandbox cs-1 and the
// codespace mine-1, which runspace did not create.
func newFakeGitHub() *sandboxtest.GitHub {
	fake := new(sandboxtest.GitHub)

	fake.Add(availableCodespace(ownedSandbox, sandbox.DisplayName))
	fake.Add(availableCodespace(foreignSandbox, "laptop"))

	return fake
}

func availableCodespace(name, displayName string) github.Codespace {
	return github.Codespace{
		Name:                   name,
		DisplayName:            displayName,
		State:                  "Available",
		Repository:             github.Repository{Name: "r", FullName: repoName},
		Machine:                nil,
		CreatedAt:              time.Time{},
		IdleTimeoutMinutes:     nil,
		RetentionPeriodMinutes: nil,
		RetentionExpiresAt:     nil,
	}
}

func newFakeSandboxes(
	fake *sandboxtest.GitHub, dialer *sandboxtest.Dialer, logger *log.Logger, reuse bool,
) (*sandbox.Sandboxes, error) {
	dial := func(ctx context.Context, name string, progress func(string)) (sandbox.Conn, error) {
		conn, err := dialer.Dial(ctx, name, progress)
		if err != nil {
			return nil, relay(err)
		}

		return conn, nil
	}

	sandboxes, err := sandbox.New(sandbox.Config{
		Token:     "",
		Getenv:    func(string) string { return "" },
		APIClient: fake.Client(),
		Dial:      dial,
		Logger:    logger,
		Reuse:     reuse,
	})

	return sandboxes, relay(err)
}

// run runs the command line args on a fresh command tree, as main does.
func (rig *harness) run(args ...string) int {
	return rig.execute(context.Background(), newRoot(rig.app), args)
}

// interrupt runs args like run, and cancels them once the command waits, as
// a first Ctrl-C does.
func (rig *harness) interrupt(args ...string) int {
	return rig.execute(cancelOnceBlocked(context.Background()), newRoot(rig.app), args)
}

func (rig *harness) execute(ctx context.Context, root *cobra.Command, args []string) int {
	rig.stdout.Reset()
	rig.stderr.Reset()

	return execute(ctx, root, rig.app.log, args)
}

// lastCmd returns the last command a fake Conn ran.
func (rig *harness) lastCmd(t *testing.T) remote.Cmd {
	t.Helper()

	conns := rig.dial.Conns()
	if len(conns) == 0 {
		t.Fatal("nothing was dialed")
	}

	cmds := conns[len(conns)-1].Cmds()
	if len(cmds) == 0 {
		t.Fatal("no command ran")
	}

	return cmds[len(cmds)-1]
}

// errorLines counts the error lines in the log on stderr, with or without
// a timestamp before the level.
func (rig *harness) errorLines() int {
	count := 0

	for line := range strings.Lines(rig.stderr.String()) {
		if strings.Contains(line, errorLevel) {
			count++
		}
	}

	return count
}

// mustRun runs the command line and fails the test unless it exits 0.
func (rig *harness) mustRun(t *testing.T, args ...string) {
	t.Helper()

	if code := rig.run(args...); code != 0 {
		t.Fatalf("%s: exit %d, %s", args[0], code, rig.stderr.String())
	}
}

func exitWith(code int) sandboxtest.Handler {
	return func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if sandboxtest.Step(cmd) != "" {
			return sandboxtest.Default(ctx, name, cmd)
		}

		return code, nil
	}
}

// Argument parsing.
func TestParsingExecSplitsNameFromCommand(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.mustRun(t, cmdExec, ownedSandbox, "ls", "-la")

	if got := rig.lastCmd(t).Argv; !slices.Equal(got, []string{"ls", "-la"}) {
		t.Errorf("argv = %q, want [ls -la]", got)
	}

	if got := rig.lastCmd(t).Stdin; got != nil {
		t.Error("stdin attached without -i")
	}
}

func TestParsingEnvKeepsCommas(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.run(cmdExec, "-e", "A=1,2", "-e", "B=3", "--", ownedSandbox, "env")

	if got := rig.lastCmd(t).Env; !slices.Equal(got, []string{"A=1,2", "B=3"}) {
		t.Errorf("env = %q", got)
	}
}

func TestParsingTTYNeedsTerminal(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)

	code := rig.run(cmdExec, "-t", ownedSandbox, "bash")
	if code != 125 || !strings.Contains(rig.stderr.String(), "-t needs stdin to be a terminal") {
		t.Errorf("-t without a terminal: exit %d, %q", code, rig.stderr.String())
	}
}

func TestParsingInteractiveTTY(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.app.tty = os.Stdin // pretend stdin is a terminal
	rig.mustRun(t, cmdExec, "-it", ownedSandbox, "bash")

	if cmd := rig.lastCmd(t); cmd.TTY == nil || cmd.Stdin == nil {
		t.Errorf("-it: TTY %v, stdin %v", cmd.TTY, cmd.Stdin)
	}
}

func TestParsingTTYImpliesInteractive(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.app.tty = os.Stdin // pretend stdin is a terminal
	rig.run(cmdExec, "-t", ownedSandbox, "bash")

	if cmd := rig.lastCmd(t); cmd.TTY == nil || cmd.Stdin == nil {
		t.Error("-t does not imply -i")
	}
}

func TestParsingRunTakesWholeCommand(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.mustRun(t, cmdRun, repoFlag, repoName, "--keep", "failure", buildCmd, "test")

	if got := rig.lastCmd(t).Argv; !slices.Equal(got, []string{buildCmd, "test"}) {
		t.Errorf("run argv = %q", got)
	}

	if !rig.gh.Called("DELETE /user/codespaces/cs-2") {
		t.Error("run with a passing command and --keep failure did not delete")
	}
}

// Declared rules.
func TestDeclaredRules(t *testing.T) {
	t.Parallel()

	rules := []struct {
		args []string
		want string
	}{
		{[]string{cmdList, "-q", "--json"}, "none of the others can be"},
		{[]string{cmdDelete, ownedSandbox, "--all"}, "not both"},
		{[]string{cmdDelete}, "give sandbox names or all"},
		{[]string{cmdDiff}, "accepts 1 arg(s)"},
		{[]string{cmdCopy, "a"}, "accepts 2 arg(s)"},
		{[]string{cmdList, "extra"}, msgUnknownCommand},
	}
	for _, rule := range rules {
		t.Run(strings.Join(rule.args, " "), func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)

			code := rig.run(rule.args...)
			if code != 1 || !strings.Contains(rig.stderr.String(), rule.want) {
				t.Errorf("%q: exit %d, stderr %q; want 1 and %q", rule.args, code, rig.stderr.String(), rule.want)
			}
		})
	}
}

// A usage error in exec or run exits 125.
func TestUsageErrorsInCommandRunners(t *testing.T) {
	t.Parallel()

	usages := [][]string{
		{cmdExec, ownedSandbox},
		{cmdRun, repoFlag, repoName},
		{cmdRun, buildCmd},
		{cmdExec, "--bogus", ownedSandbox, "x"},
	}
	for _, args := range usages {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			if code := rig.run(args...); code != 125 {
				t.Errorf("%q: exit %d, want 125", args, code)
			}
		})
	}
}

// exitRow is one row of the exit-code table.
type exitRow struct {
	name    string
	handler sandboxtest.Handler
	args    []string
	code    int
	lines   int
	message string
}

func exitRows() []exitRow {
	return []exitRow{
		{"success", nil, []string{cmdExec, ownedSandbox, "true"}, 0, 0, ""},
		{"remote 7", exitWith(7), []string{cmdExec, ownedSandbox, "x"}, 7, 0, ""},
		{"remote 127", exitWith(127), []string{cmdExec, ownedSandbox, "x"}, 127, 0, ""},
		{"runspace failure in exec", nil, []string{cmdExec, foreignSandbox, "x"}, 125, 1, msgNotOurs},
		{"usage error in exec", nil, []string{cmdExec, "-e", "1=x", ownedSandbox, "x"}, 125, 1, "invalid env key"},
		{"env without a value", nil, []string{cmdExec, "-e", "HOME", ownedSandbox, "x"}, 125, 1, `invalid env "HOME"`},
		{"bare env in run", nil, []string{cmdRun, repoFlag, repoName, "-e", "HOME", "x"}, 125, 1, `invalid env "HOME"`},
		{"failure in another command", nil, []string{cmdDiff, foreignSandbox}, 1, 1, msgNotOurs},
		{"remote 3 in run", exitWith(3), []string{cmdRun, repoFlag, repoName, "x"}, 3, 0, ""},
		{"unknown command", nil, []string{"bogus"}, 1, 1, msgUnknownCommand},
		{"ls succeeds", nil, []string{cmdList}, 0, 0, ""},
	}
}

// The exit-code table, row by row.
func TestExitMapping(t *testing.T) {
	t.Parallel()

	for _, row := range exitRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, row.handler)

			code := rig.run(row.args...)
			if code != row.code || rig.errorLines() != row.lines || !strings.Contains(rig.stderr.String(), row.message) {
				t.Errorf("exit %d, stderr %q; want %d with %d error line(s)",
					code, rig.stderr.String(), row.code, row.lines)
			}
		})
	}
}

// blockingHandler runs the setup steps, then blocks the command itself until
// its context ends.
func blockingHandler(blocked func(step string) bool) sandboxtest.Handler {
	return func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if !blocked(sandboxtest.Step(cmd)) {
			return sandboxtest.Default(ctx, name, cmd)
		}

		<-ctx.Done()

		return 0, context.Cause(ctx)
	}
}

// cancelOnceBlocked returns a context that is canceled once every goroutine
// in the synctest bubble is blocked, that is, once the command is waiting.
func cancelOnceBlocked(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)

	go func() {
		synctest.Wait()
		cancel()
	}()

	return ctx
}

func TestExitTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newHarness(t, blockingHandler(func(step string) bool { return step == "" }))

		code := rig.run(cmdExec, "--timeout", "1m", ownedSandbox, "sleep")
		if code != 124 || rig.errorLines() != 1 {
			t.Errorf("timeout: exit %d, %q", code, rig.stderr.String())
		}

		code = rig.run(cmdRun, repoFlag, repoName, "--timeout", "1m", "sleep")
		if code != 124 || !strings.Contains(rig.stderr.String(), "timeout") {
			t.Errorf("run timeout: exit %d, %q", code, rig.stderr.String())
		}
	})
}

// Commands other than exec and run exit 0 or 1, timeouts included.
func TestDiffTimeoutExitsOne(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newHarness(t, blockingHandler(func(step string) bool { return step == cmdDiff }))

		code := rig.run(cmdDiff, "--timeout", "1m", ownedSandbox)
		if code != 1 || !strings.Contains(rig.stderr.String(), "did not finish within the 1m timeout") {
			t.Errorf("diff timeout: exit %d, %q", code, rig.stderr.String())
		}
	})
}

func TestExitInterrupt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newHarness(t, blockingHandler(func(step string) bool { return step == "" }))

		code := rig.interrupt(cmdExec, ownedSandbox, "sleep")
		if code != 130 || !strings.Contains(rig.stderr.String(), "interrupted") {
			t.Errorf("interrupt: exit %d, %q", code, rig.stderr.String())
		}

		code = rig.interrupt(cmdRun, repoFlag, repoName, "sleep")
		if code != 130 {
			t.Errorf("run interrupt: exit %d, %q", code, rig.stderr.String())
		}
	})
}

// -V and --version print the version; -v is --verbose.
func TestVersion(t *testing.T) {
	t.Parallel()

	for _, flag := range []string{"-V", "--version"} {
		rig := newHarness(t, nil)

		code := rig.run(flag)
		if code != 0 || !strings.HasPrefix(rig.stdout.String(), "runspace version ") {
			t.Errorf("%s: exit %d, %q", flag, code, rig.stdout.String())
		}
	}
}

// requireContains fails the test for each want that got lacks.
func requireContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s: %s", what, want, got)
		}
	}
}

func TestListOutput(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.run(cmdList, "-q")

	if got := rig.stdout.String(); got != "cs-1\n" {
		t.Errorf("ls -q = %q", got)
	}

	rig.run(cmdList)
	requireContains(t, "ls", rig.stdout.String(), "CREATED", "cs-1", "00:00:00 UTC")

	rig.run(cmdList, "--json")
	requireContains(t, "ls --json", rig.stdout.String(),
		`"name":"cs-1"`, `"state":"Available"`, `"repository":"o/r"`, `"machine":""`,
		`"created_at"`, `"retention_expires_at"`)

	rig.run(cmdCreate, repoFlag, repoName, "--json")
	requireContains(t, "create --json", rig.stdout.String(),
		`"name":"cs-2"`, `"repository":"o/r"`, `"workspace":"/workspaces/r"`, `"base":"`+sandboxtest.HeadSHA+`"`)
}

func TestRunNotices(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, exitWith(3))
	if code := rig.run(cmdRun, repoFlag, repoName, "--keep", "failure", "x"); code != 3 {
		t.Fatalf("exit %d", code)
	}

	requireContains(t, "stderr", rig.stderr.String(),
		infoLevel+"Kept the sandbox, stopped", "name=cs-2", `inspect="runspace exec -t cs-2 bash"`)

	// A failed delete is a warning that says how to remove the sandbox.
	rig = newHarness(t, nil)
	rig.gh.Fail = map[string]int{"DELETE /user/codespaces/cs-2": 500}
	rig.mustRun(t, cmdRun, repoFlag, repoName, "x")

	requireContains(t, "stderr", rig.stderr.String(),
		warnLevel+"Could not delete the sandbox", "name=cs-2", `remove="runspace delete cs-2"`)

	// A kept sandbox that could not be stopped gets only the warning, with the
	// hints for inspecting and removing it.
	rig = newHarness(t, exitWith(3))
	rig.gh.Fail = map[string]int{"POST /user/codespaces/cs-2/stop": 500}

	if code := rig.run(cmdRun, repoFlag, repoName, "--keep", "failure", "x"); code != 3 {
		t.Fatalf("exit %d", code)
	}

	stderr := rig.stderr.String()
	requireContains(t, "stderr", stderr,
		warnLevel+"Could not stop the kept sandbox", "name=cs-2",
		`inspect="runspace exec -t cs-2 bash"`, `remove="runspace delete cs-2"`)

	if strings.Contains(stderr, "Kept the sandbox, stopped") {
		t.Errorf("stderr reports a sandbox that failed to stop as stopped:\n%s", stderr)
	}
}

// withOrigin makes dir a git repository whose remote origin is url, and
// returns dir.
func withOrigin(t *testing.T, dir, url string) string {
	t.Helper()

	gitDir := filepath.Join(dir, ".git")

	err := os.MkdirAll(gitDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	config := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + url + "\n"

	err = os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

const (
	originNotice = infoLevel + "Using o/r from git remote origin"
	buildCmd     = "make"
)

// Without -R, create and run use the git remote origin of the working
// directory, and say so.
func TestRepoDefaultsToOrigin(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{cmdCreate}, {cmdRun, buildCmd}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			// A subdirectory of the repository, which the search walks up from.
			sub := filepath.Join(withOrigin(t, rig.app.dir, "https://github.com/o/r.git"), "pkg", "x")

			err := os.MkdirAll(sub, 0o750)
			if err != nil {
				t.Fatal(err)
			}

			rig.app.dir = sub
			rig.mustRun(t, args...)

			if !rig.gh.Called("POST /repos/o/r/codespaces") {
				t.Errorf("no codespace was created for o/r: %v", rig.gh.Calls())
			}

			requireContains(t, "stderr", rig.stderr.String(), originNotice)
		})
	}
}

// Outside a git repository, create exits 1 and run 125, and both say to
// pass -R.
func TestRepoOutsideGitRepositoryNeedsFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args []string
		code int
	}{
		{[]string{cmdCreate}, 1},
		{[]string{cmdRun, buildCmd}, 125},
	}
	for _, test := range tests {
		t.Run(test.args[0], func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)

			code := rig.run(test.args...)
			if code != test.code {
				t.Errorf("exit %d, want %d", code, test.code)
			}

			requireContains(t, "stderr", rig.stderr.String(), "not in a git repository", flagHint)

			if len(rig.gh.Calls()) != 0 {
				t.Errorf("GitHub was called: %v", rig.gh.Calls())
			}
		})
	}
}

// -R wins over the git remote origin, and then there is no notice.
func TestRepoFlagBeatsOrigin(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	withOrigin(t, rig.app.dir, "https://github.com/other/repo.git")
	rig.mustRun(t, cmdCreate, repoFlag, repoName)

	if !rig.gh.Called("POST /repos/o/r/codespaces") {
		t.Errorf("no codespace was created for o/r: %v", rig.gh.Calls())
	}

	if strings.Contains(rig.stderr.String(), "git remote origin") {
		t.Errorf("notice with -R: %q", rig.stderr.String())
	}
}

// A remote that is not on GitHub is an error that names neither the URL nor
// its credentials.
func TestRepoOriginElsewhere(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	withOrigin(t, rig.app.dir, "https://user:SECRET@gitlab.com/o/r.git")

	if code := rig.run(cmdCreate); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}

	requireContains(t, "stderr", rig.stderr.String(), "gitlab.com", flagHint)

	for _, leak := range []string{"SECRET", "user", "o/r"} {
		if strings.Contains(rig.stderr.String(), leak) {
			t.Errorf("stderr leaks %q: %q", leak, rig.stderr.String())
		}
	}
}

// The notice goes to stderr only, so --json output stays pure JSON.
func TestRepoDefaultKeepsJSONOutputPure(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	withOrigin(t, rig.app.dir, "https://github.com/o/r.git")
	rig.mustRun(t, cmdCreate, "--json")

	if !json.Valid(rig.stdout.Bytes()) || strings.Contains(rig.stdout.String(), "using") {
		t.Errorf("stdout = %q, want only JSON", rig.stdout.String())
	}

	requireContains(t, "stderr", rig.stderr.String(), originNotice)
}

// An origin with a terminal escape in its name is refused, and nothing
// writes the escape to stderr.
func TestRepoOriginEscapeNeverReachesStderr(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	withOrigin(t, rig.app.dir, "https://github.com/o%1b%5b2J/r")

	if code := rig.run(cmdCreate); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}

	if strings.Contains(rig.stderr.String(), "\x1b") || !strings.Contains(rig.stderr.String(), flagHint) {
		t.Errorf("stderr = %q", rig.stderr.String())
	}
}

const flagToken = "flag-token"

// -T/--token reaches Sandboxes wherever it is on the command line; without
// it, "" selects RUNSPACE_TOKEN.
func TestTokenFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-T", flagToken, cmdList}, flagToken},
		{[]string{cmdList, "--token", flagToken}, flagToken},
		{[]string{"--token=" + flagToken, cmdList}, flagToken},
		{[]string{cmdList}, ""},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			rig.mustRun(t, test.args...)

			if !slices.Equal(rig.tokens, []string{test.want}) {
				t.Errorf("tokens = %q, want [%q]", rig.tokens, test.want)
			}
		})
	}
}

// After NAME or CMD, -T belongs to the remote command and is not the token.
func TestTokenFlagAfterCommandIsRemote(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{cmdExec, ownedSandbox, buildCmd, "-T", flagToken},
		{cmdRun, repoFlag, repoName, buildCmd, "-T", flagToken},
	} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			rig.mustRun(t, args...)

			if got, want := rig.lastCmd(t).Argv, []string{buildCmd, "-T", flagToken}; !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}

			if !slices.Equal(rig.tokens, []string{""}) {
				t.Errorf("tokens = %q, want [\"\"]", rig.tokens)
			}
		})
	}
}

// exec keeps -t for a terminal next to -T.
func TestTokenFlagBesideExecTTY(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.app.tty = os.Stdin // pretend stdin is a terminal
	rig.mustRun(t, cmdExec, "-T", flagToken, "-it", ownedSandbox, "bash")

	if cmd := rig.lastCmd(t); cmd.TTY == nil || cmd.Stdin == nil {
		t.Errorf("-it: TTY %v, stdin %v", cmd.TTY, cmd.Stdin)
	}

	if !slices.Equal(rig.tokens, []string{flagToken}) {
		t.Errorf("tokens = %q, want [%q]", rig.tokens, flagToken)
	}
}

var errStopBeforeServing = errors.New("stop before serving")

// The MCP server uses -T for every tool call.
func TestTokenFlagServesMCP(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)

	var got string

	rig.app.newSandboxes = func(token string, _ bool) (*sandbox.Sandboxes, error) {
		got = token

		return nil, errStopBeforeServing
	}

	if code := rig.run("mcp", "-T", flagToken); code != 1 || got != flagToken {
		t.Errorf("exit %d, token %q; want 1, %q", code, got, flagToken)
	}
}

// An empty --token, as from an unset variable, fails before any Sandboxes
// exist, rather than falling back to RUNSPACE_TOKEN.
func TestTokenFlagEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args []string
		code int
	}{
		{[]string{"--token", "", cmdList}, 1},
		{[]string{cmdExec, "-T", "", ownedSandbox, buildCmd}, 125},
	}
	for _, test := range tests {
		t.Run(test.args[0], func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)

			code := rig.run(test.args...)
			if code != test.code {
				t.Errorf("exit %d, want %d", code, test.code)
			}

			requireContains(t, "stderr", rig.stderr.String(), "--token is empty")

			if len(rig.tokens) != 0 {
				t.Errorf("Sandboxes opened with %q", rig.tokens)
			}
		})
	}
}
