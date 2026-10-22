//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/log/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.sandbox.ec/runspace/internal/mcpserver"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// waitLimit is how long a test waits for something that must happen.
const waitLimit = 10 * time.Second

// assertDials fails the test unless name has been dialed want times.
func assertDials(t *testing.T, fix *fixture, name string, want int, when string) {
	t.Helper()

	if got := fix.dialer.Dials(name); got != want {
		t.Errorf("dials = %d %s, want %d", got, when, want)
	}
}

// assertClosed fails the test with failure unless conn is closed.
func assertClosed(t *testing.T, conn *sandboxtest.Conn, failure string) {
	t.Helper()

	select {
	case <-conn.Done():
	default:
		t.Error(failure)
	}
}

// runConcurrently calls run from count goroutines and fails the test for
// each tool error.
func runConcurrently(t *testing.T, count int, run func() *mcp.CallToolResult) {
	t.Helper()

	var group sync.WaitGroup
	for range count {
		group.Go(func() {
			if res := run(); res.IsError {
				t.Error(text(res))
			}
		})
	}

	group.Wait()
}

// One dial per name under concurrent calls, an idle close on
// a fake clock, reconnection after a transport error without a retry, and
// delete closing the cached Conn.
func TestConnections(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var (
			failNext atomic.Bool
			runs     atomic.Int32
		)

		fix := newFixture(t, userCmd(func(context.Context, remote.Cmd) (int, error) {
			runs.Add(1)

			if failNext.CompareAndSwap(true, false) {
				return 0, errConnectionLost
			}

			return 0, nil
		}))
		name := fix.create(t)
		run := func() *mcp.CallToolResult { return fix.call(t, toolExec, execArgs(name, "x")) }

		runConcurrently(t, 20, run)
		assertDials(t, fix, name, 1, "after concurrent calls")

		first := fix.dialer.Conns()[0]

		time.Sleep(11 * time.Minute)
		synctest.Wait()
		assertClosed(t, first, "idle Conn not closed after 10 minutes")

		run()
		assertDials(t, fix, name, 2, "after the idle close")

		failNext.Store(true)

		before := runs.Load()

		if res := run(); !res.IsError {
			t.Error("transport error not reported")
		}

		if got := runs.Load() - before; got != 1 {
			t.Errorf("command ran %d times, want 1", got)
		}

		run()
		assertDials(t, fix, name, 3, "after a transport error")

		last := fix.dialer.Conns()[2]
		fix.call(t, toolDelete, map[string]any{"names": []string{name}})
		assertClosed(t, last, "delete left the cached Conn open")
	})
}

// drain returns the progress messages received so far.
func drain(progress chan string) []string {
	var messages []string
	for len(progress) > 0 {
		messages = append(messages, <-progress)
	}

	return messages
}

// Create sends progress notifications when the client asks.
func TestProgress(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fix := newFixture(t, nil)
		fix.github.States = []string{"Queued", stateProvisioning, "Available"}
		params := toolCall(toolCreate, map[string]any{keyRepo: testRepo})
		params.SetProgressToken("p1")

		if res := callWith(t, fix.client, params); res.IsError {
			t.Fatalf("create: %s", text(res))
		}

		synctest.Wait()

		got := strings.Join(drain(fix.progress), "|")
		if !strings.Contains(got, "Creating") || !strings.Contains(got, stateProvisioning) {
			t.Errorf("progress = %q", got)
		}
	})
}

// assertCreateCancelledDeletes cancels a create in the middle of its POST
// and fails the test unless the codespace is deleted.
func assertCreateCancelledDeletes(t *testing.T, fix *fixture) {
	t.Helper()

	fix.github.States = []string{stateProvisioning}
	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	fix.github.OnCreate = cancel

	_, _ = fix.client.CallTool(ctx, toolCall(toolCreate, map[string]any{keyRepo: testRepo}))

	deadline := time.Now().Add(5 * time.Second)
	for !fix.github.Called("DELETE /user/codespaces/cs-2") {
		if time.Now().After(deadline) {
			t.Fatal("a cancelled create left its codespace")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// A cancelled exec cancels the remote command, and a
// cancelled create deletes its codespace.
func TestCancellation(t *testing.T) {
	t.Parallel()

	started, cancelled := make(chan struct{}), make(chan struct{})
	fix := newFixture(t, userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		close(started)

		code, err := blockUntilCancelled(ctx, cmd)

		close(cancelled)

		return code, err
	}))
	name := fix.create(t)
	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	go func() {
		<-started
		cancel()
	}()

	_, _ = fix.client.CallTool(ctx, toolCall(toolExec, execArgs(name, cmdSleep)))

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the remote command was not cancelled")
	}

	assertCreateCancelledDeletes(t, fix)
}

// A long command reports progress every minute.
func TestHeartbeat(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fix := newFixture(t, userCmd(func(context.Context, remote.Cmd) (int, error) {
			time.Sleep(3*mcpserver.HeartbeatEvery + time.Second)

			return 0, nil
		}))
		name := fix.create(t)
		drain(fix.progress)

		params := toolCall(toolExec, execArgs(name, cmdLong))
		params.SetProgressToken("hb")

		if res := callWith(t, fix.client, params); res.IsError {
			t.Fatalf("exec: %s", text(res))
		}

		synctest.Wait()

		if got := len(fix.progress); got < 3 {
			t.Errorf("%d heartbeats over 3 minutes, want 3", got)
		}
	})
}

// serving is Serve running over pipes, with a client connected to it.
type serving struct {
	client *mcp.ClientSession
	// stdin is the client's output; closing it is EOF for the server.
	stdin *io.PipeWriter
	done  chan error
	// logs holds the server's log, at DebugLevel.
	logs *lockedBuffer
}

// serve starts Serve on sandboxes, logging at level, and connects a client
// to it.
func serve(ctx context.Context, t *testing.T, sandboxes *sandbox.Sandboxes, level log.Level) *serving {
	t.Helper()

	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	logger, logs := newBufferLogger(level)

	go func() {
		done <- mcpserver.Serve(ctx, sandboxes, logger, "test", serverIn, serverOut)
		// A real server's stdout closes when the process exits.
		_ = serverOut.Close()
	}()

	transport := &mcp.IOTransport{Reader: clientIn, Writer: clientOut, MaxLineLength: 0}

	client, err := mcp.NewClient(clientInfo(), nil).Connect(context.WithoutCancel(ctx), transport, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return &serving{client: client, stdin: clientOut, done: done, logs: logs}
}

// result waits for Serve to return and returns its error. It fails the test
// with failure if Serve does not return.
func (s *serving) result(t *testing.T, failure string) error {
	t.Helper()

	select {
	case err := <-s.done:
		return err
	case <-time.After(waitLimit):
		t.Fatal(failure)

		return nil
	}
}

// Stdin EOF cancels calls in flight, so a pending create deletes its
// codespace, closes every Conn, and returns.
func TestShutdownOnEOF(t *testing.T) {
	t.Parallel()

	github := new(sandboxtest.GitHub)
	started := make(chan struct{}, 1)
	dialer := newDialer(github, userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		started <- struct{}{}

		return blockUntilCancelled(ctx, cmd)
	}))
	server := serve(context.Background(), t, newSandboxes(t, github, dialer, nil), log.DebugLevel)

	res := callWith(t, server.client, toolCall(toolCreate, map[string]any{keyRepo: testRepo}))
	if res.IsError {
		t.Fatalf("create: %s", text(res))
	}

	name := sandboxName(t, res)

	callInBackground(server.client, toolExec, execArgs(name, cmdLong))

	<-started

	github.States = []string{stateProvisioning}
	posted := make(chan struct{})
	github.OnCreate = func() { close(posted) }

	callInBackground(server.client, toolCreate, map[string]any{keyRepo: testRepo})

	<-posted

	_ = server.stdin.Close()

	_ = server.result(t, "Serve did not return after stdin EOF")

	for _, conn := range dialer.Conns() {
		assertClosed(t, conn, "Conn to "+conn.Name+" left open")
	}

	if !github.Called("DELETE /user/codespaces/cs-2") {
		t.Error("the pending create left its codespace")
	}

	if _, ok := github.Codespace(name); !ok {
		t.Error("shutdown deleted a finished sandbox")
	}
}

// Ending Serve's context, as a signal does, cancels the calls in flight,
// and Serve returns nil.
func TestServeCancelledBySignal(t *testing.T) {
	t.Parallel()

	github := new(sandboxtest.GitHub)
	started := make(chan struct{})
	dialer := newDialer(github, userCmd(func(ctx context.Context, cmd remote.Cmd) (int, error) {
		close(started)

		return blockUntilCancelled(ctx, cmd)
	}))
	serveCtx, stop := context.WithCancel(context.Background())

	defer stop()

	server := serve(serveCtx, t, newSandboxes(t, github, dialer, nil), log.DebugLevel)

	res := callWith(t, server.client, toolCall(toolCreate, map[string]any{keyRepo: testRepo}))
	if res.IsError {
		t.Fatalf("create: %s", text(res))
	}

	callInBackground(server.client, toolExec, execArgs(sandboxName(t, res), cmdLong))

	<-started

	stop()

	err := server.result(t, "Serve did not return after its context ended")
	if err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
}

// The SDK's own log goes to the logger that Serve was given, at
// DebugLevel.
func TestServeLogsTheSDK(t *testing.T) {
	t.Parallel()

	github := new(sandboxtest.GitHub)
	server := serve(context.Background(), t, newSandboxes(t, github, newDialer(github, nil), nil), log.DebugLevel)
	_ = server.stdin.Close()

	err := server.result(t, "Serve did not return on EOF")
	if err != nil {
		t.Fatal(err)
	}

	if logs := server.logs.String(); !strings.Contains(logs, "DEBUG server run start") {
		t.Errorf("the log lacks the SDK's start line: %s", logs)
	}
}

// At InfoLevel a server that a signal ends logs nothing, though the SDK
// logs its cancellation as an error.
func TestServeQuietByDefault(t *testing.T) {
	t.Parallel()

	serveCtx, stop := context.WithCancel(context.Background())
	github := new(sandboxtest.GitHub)
	server := serve(serveCtx, t, newSandboxes(t, github, newDialer(github, nil), nil), log.InfoLevel)

	stop()

	err := server.result(t, "Serve did not return after its context ended")
	if err != nil {
		t.Fatal(err)
	}

	if logs := server.logs.String(); logs != "" {
		t.Errorf("the log = %q, want nothing", logs)
	}
}
