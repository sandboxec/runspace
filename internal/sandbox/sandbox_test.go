//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

const untaggedName = "mine-1"

// Every name-taking command, ls, and delete --all act only on
// tagged codespaces.
func TestOnlyTaggedCodespaces(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, false)
	rig.gh.Add(github.Codespace{
		Name:                   untaggedName,
		DisplayName:            "my laptop",
		State:                  "Available",
		Repository:             github.Repository{Name: "r", FullName: testRepo},
		Machine:                nil,
		CreatedAt:              time.Time{},
		IdleTimeoutMinutes:     nil,
		RetentionPeriodMinutes: nil,
		RetentionExpiresAt:     nil,
	})

	tagged := rig.mustCreate(ctx, t)

	refused := func(what string, err error) {
		t.Helper()

		if err == nil || !strings.Contains(err.Error(), "not created by runspace") {
			t.Errorf("%s on an untagged codespace: %v", what, err)
		}
	}
	refused("exec", rig.Exec(ctx, newExec(untaggedName, newCommand("x")), noStreams()))

	_, err := rig.Diff(ctx, newDiff(untaggedName, ""), streamsTo(new(bytes.Buffer)))
	refused("diff", err)

	_, err = rig.Copy(ctx, newCopy(untaggedName+":x", filepath.Join(t.TempDir(), "x")), noStreams())
	refused("cp", err)

	_, err = rig.Delete(ctx, newDelete(untaggedName))
	refused("delete", err)

	if n := rig.dial.Dials(untaggedName); n != 0 {
		t.Errorf("dialed an untagged codespace %d times", n)
	}

	list, err := rig.List(ctx, sandbox.ListOptions{})
	if err != nil || len(list) != 1 || list[0].Name != tagged {
		t.Errorf("ls = %+v, %v; want only %s", list, err, tagged)
	}

	deleted, err := rig.Delete(ctx, deleteAll())
	if err != nil || !slices.Equal(deleted, []string{tagged}) {
		t.Errorf("delete --all = %v, %v", deleted, err)
	}

	if _, ok := rig.gh.Codespace(untaggedName); !ok {
		t.Error("delete --all removed an untagged codespace")
	}
}

// A command leaves the lookup to the dial, whose connection query reports
// the tag too, so it makes no REST call of its own.
func TestCommandsLookUpOnlyThroughTheDial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, false)
	name := rig.mustCreate(ctx, t)
	before := len(rig.gh.Calls())

	err := rig.Exec(ctx, newExec(name, newCommand(cmdTrue)), noStreams())
	if err != nil {
		t.Fatal(err)
	}

	if calls := rig.gh.Calls()[before:]; len(calls) != 0 {
		t.Errorf("exec called the REST API: %v", calls)
	}
}

// diffFailures are remote diffs that must leave no file behind.
func diffFailures() map[string]sandboxtest.Handler {
	failAfterPartial := func(code int, end error) sandboxtest.Handler {
		return userStepOrDiff(func(cmd remote.Cmd) (int, error) {
			_, err := io.WriteString(cmd.Stdout, "partial")
			if err != nil {
				return 0, fmt.Errorf("write partial patch: %w", err)
			}

			return code, end
		})
	}

	return map[string]sandboxtest.Handler{
		"remote exit": failAfterPartial(1, nil),
		"dropped":     failAfterPartial(0, remote.ErrNoExitStatus),
	}
}

// userStepOrDiff answers runspace's prepare step, and runs onDiff for diff.
func userStepOrDiff(onDiff func(cmd remote.Cmd) (int, error)) sandboxtest.Handler {
	return func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if sandboxtest.Step(cmd) == stepDiff {
			return onDiff(cmd)
		}

		return runspaceStep(ctx, name, cmd)
	}
}

// diff -o leaves no file when the remote fails or the
// connection drops.
func TestDiffOutputFailureLeavesNoFile(t *testing.T) {
	t.Parallel()

	for name, handler := range diffFailures() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, false)
			rig.dial.Handler = handler
			sandboxName := rig.mustCreate(context.Background(), t)
			dir := t.TempDir()

			_, err := rig.Diff(context.Background(), newDiff(sandboxName, filepath.Join(dir, "out.patch")), noStreams())
			if err == nil {
				t.Error("diff succeeded")
			}

			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("left %v", entries)
			}
		})
	}
}

// diff -o writes the patch atomically.
func TestDiffOutputIsAtomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, false)
	sandboxName := rig.mustCreate(ctx, t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.patch")
	writeFile(t, out, "old", 0o644)

	got, err := rig.Diff(ctx, newDiff(sandboxName, out), noStreams())
	if err != nil || got != out {
		t.Fatalf("Diff = %q, %v", got, err)
	}

	patch, _ := os.ReadFile(filepath.Clean(out))
	if string(patch) != sandboxtest.Patch {
		t.Errorf("patch = %q", patch)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir = %v, want only out.patch", entries)
	}

	var stdout bytes.Buffer

	_, err = rig.Diff(ctx, newDiff(sandboxName, ""), streamsTo(&stdout))
	if err != nil || stdout.String() != sandboxtest.Patch {
		t.Errorf("stdout patch = %q, %v", stdout.String(), err)
	}

	cmds := rig.dial.Conns()[len(rig.dial.Conns())-1].Cmds()

	script := cmds[len(cmds)-1].Argv[2]
	if !strings.Contains(script, "GIT_INDEX_FILE") || !strings.Contains(script, "--src-prefix=a/") {
		t.Errorf("diff script lacks its pinned settings:\n%s", script)
	}
}

// writeFile writes content to path or fails the test.
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	err := os.WriteFile(path, []byte(content), mode)
	if err != nil {
		t.Fatal(err)
	}
}

// runspace writes nothing to HOME or the working directory,
// with per-call connections (CLI) or cached ones (MCP).
func TestNoHostState(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprint("reuse=", reuse), func(t *testing.T) {
			home, workDir := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(workDir)

			ctx := context.Background()
			rig := newFixture(t, reuse)
			name := rig.mustCreate(ctx, t)

			err := rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
			if err != nil {
				t.Fatal(err)
			}

			_, err = rig.Diff(ctx, newDiff(name, ""), streamsTo(new(bytes.Buffer)))
			if err != nil {
				t.Fatal(err)
			}

			_, err = rig.Delete(ctx, newDelete(name))
			if err != nil {
				t.Fatal(err)
			}

			for _, dir := range []string{home, workDir} {
				entries, _ := os.ReadDir(dir)
				if len(entries) != 0 {
					t.Errorf("%s holds %v", dir, entries)
				}
			}
		})
	}
}

// cacheTest is the connection cache under test: reuse is on, and the create
// Conn is dropped, so the first call dials afresh.
type cacheTest struct {
	t    *testing.T
	rig  *fixture
	name string
}

func newCacheTest(t *testing.T) *cacheTest {
	t.Helper()

	rig := newFixture(t, true)
	// "long" holds its call open until its stdin ends.
	rig.dial.Handler = userCmd(func(_ context.Context, cmd remote.Cmd) (int, error) {
		if cmd.Argv[0] == cmdLong {
			_, err := io.Copy(io.Discard, cmd.Stdin)
			if err != nil {
				return 0, fmt.Errorf("wait for stdin: %w", err)
			}
		}

		return 0, nil
	})
	name := rig.mustCreate(context.Background(), t)
	// The create Conn is adopted. Drop it to exercise a fresh dial.
	rig.Forget(name)

	return &cacheTest{t: t, rig: rig, name: name}
}

// exec runs argv without a timeout, so only the cache decides how long the
// Conn lives.
func (c *cacheTest) exec(streams sandbox.Streams, argv ...string) error {
	cmd := withTimeout(newCommand(argv...), "0")

	err := c.rig.Exec(context.Background(), newExec(c.name, cmd), streams)
	if err != nil {
		return fmt.Errorf("exec %v: %w", argv, err)
	}

	return nil
}

// The connection cache: one dial per name under concurrent calls.
func (c *cacheTest) sharedDial() {
	c.t.Helper()

	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			err := c.exec(noStreams(), "x")
			if err != nil {
				c.t.Error(err)
			}
		})
	}

	group.Wait()

	if n := c.rig.dial.Dials(c.name); n != 2 {
		c.t.Fatalf("dials = %d, want 2 (create, then one shared)", n)
	}
}

// A long call keeps the Conn open past the idle timeout.
func (c *cacheTest) longCallKeepsConn(conn *sandboxtest.Conn) {
	c.t.Helper()

	release := make(chan struct{})
	longDone := make(chan error)

	go func() { longDone <- c.exec(streamsFrom(&gate{release: release}), cmdLong) }()

	synctest.Wait()
	time.Sleep(3 * sandbox.IdleClose)

	if closed(conn) {
		c.t.Fatal("idle timer closed a Conn with a call in flight")
	}

	close(release)

	err := <-longDone
	if err != nil {
		c.t.Fatal(err)
	}
}

// Idle: closed after IdleClose without a call.
func (c *cacheTest) idleCloses(conn *sandboxtest.Conn) {
	c.t.Helper()

	time.Sleep(sandbox.IdleClose - time.Second)

	if closed(conn) {
		c.t.Fatal("closed early")
	}

	time.Sleep(2 * time.Second)
	synctest.Wait()

	if !closed(conn) {
		c.t.Fatal("idle Conn not closed")
	}
}

// The next call dials again, and delete closes it.
func (c *cacheTest) redialAndDelete() {
	c.t.Helper()

	err := c.exec(noStreams(), "x")
	if err != nil {
		c.t.Fatal(err)
	}

	third := c.rig.dial.Conns()[2]

	_, err = c.rig.Delete(context.Background(), newDelete(c.name))
	if err != nil {
		c.t.Fatal(err)
	}

	if !closed(third) {
		c.t.Error("delete left the cached Conn open")
	}
}

// The connection cache: one dial per name under concurrent calls, an idle
// close that waits for in-flight calls, and delete closing the Conn.
func TestConnectionCache(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cache := newCacheTest(t)

		cache.sharedDial()

		conn := cache.rig.dial.Conns()[1]
		cache.longCallKeepsConn(conn)
		cache.idleCloses(conn)
		cache.redialAndDelete()
	})
}

// gate is a reader that blocks until release is closed.
type gate struct{ release <-chan struct{} }

func (g *gate) Read([]byte) (int, error) {
	<-g.release

	return 0, io.EOF
}

// A cached Conn that its keepalive closed is dialed again, so the next call
// does not fail. The command never started, so this is not a retry.
func TestDeadCachedConnRedials(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, true)
	name := rig.mustCreate(ctx, t)

	err := rig.dial.Conns()[0].Close() // as a failed keepalive would
	if err != nil {
		t.Fatal(err)
	}

	err = rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	if err != nil {
		t.Fatalf("exec after the Conn died: %v", err)
	}

	if n := rig.dial.Dials(name); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}

// A delete that closes the cached Conn interrupts an exec running on it;
// the exec fails as a runspace failure and nothing hangs.
func TestDeleteInterruptsExec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, true)
	started := make(chan struct{})
	rig.dial.Handler = userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		close(started)

		return blockUntilDone(ctx, cmd)
	})
	name := rig.mustCreate(ctx, t)

	done := make(chan error)

	go func() {
		cmd := withTimeout(newCommand(cmdLong), "0")
		done <- rig.Exec(ctx, newExec(name, cmd), noStreams())
	}()

	<-started

	_, err := rig.Delete(ctx, newDelete(name))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if sandbox.ExitCode(err) != sandbox.ExitFailure {
			t.Errorf("exec after delete: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec still running after delete closed its Conn")
	}
}

// cfg.Token wins, and then RUNSPACE_TOKEN is not read; without it,
// RUNSPACE_TOKEN is the fallback.
func TestNewTokenPrecedence(t *testing.T) {
	t.Parallel()

	newSandboxes := func(token string, getenv func(string) string) {
		t.Helper()

		sandboxes, err := sandbox.New(sandbox.Config{
			Token:     token,
			Getenv:    getenv,
			APIClient: nil,
			Dial:      nil,
			Logger:    nil,
			Reuse:     false,
		})
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = sandboxes.Close() })
	}

	newSandboxes("given", func(key string) string {
		t.Errorf("New read %s despite a given token", key)

		return ""
	})

	newSandboxes("", func(key string) string {
		if key == github.TokenEnv {
			return "from-env"
		}

		return ""
	})
}

// Without cfg.Token or RUNSPACE_TOKEN, New fails and names both.
func TestNewWithoutToken(t *testing.T) {
	t.Parallel()

	_, err := sandbox.New(sandbox.Config{
		Token:     "",
		Getenv:    func(string) string { return "" },
		APIClient: nil,
		Dial:      nil,
		Logger:    nil,
		Reuse:     false,
	})
	if err == nil || !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), github.TokenEnv) {
		t.Fatalf("New error = %v, want one naming --token and %s", err, github.TokenEnv)
	}
}
