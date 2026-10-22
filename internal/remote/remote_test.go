//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"go.sandbox.ec/runspace/internal/remote"
)

// Behaviors of testServer, named by argv[0], that tests refer to elsewhere.
const (
	behaviorExit = "exit"
	behaviorPty  = "pty"
	behaviorCat  = "cat"
	behaviorHang = "hang"

	// workDir is the remote directory of every test command.
	workDir = "/w"

	// settleTimeout bounds how long a test waits for something to happen.
	settleTimeout = 5 * time.Second
)

var (
	errStop       = errors.New("stop")
	errDiskFull   = errors.New("disk full")
	errUnknownKey = errors.New("unknown key")
)

// testServer is an in-process SSH server. Each session runs the behavior
// named by argv[0] of the command it receives:
//
//	exit N  exits with status N
//	kill    dies by SIGKILL
//	drop    closes without an exit status
//	cat     copies stdin to stdout, then exits 0
//	pty     prints "pty" or "nopty", then exits 0
//	sleep   waits for a signal, then dies by it
//	hang    ignores signals and waits for the session to close
//	flood   writes output until the session closes
type testServer struct {
	sessions    atomic.Int32
	ptySize     atomic.Value // "COLSxROWS" of the last pty-req
	envRequests atomic.Int32
	signals     chan string
	// silent makes the server ignore global requests, so keepalives get no
	// reply.
	silent bool
}

func newTestServer() *testServer {
	return newConfiguredServer(false, nil)
}

// newSilentServer returns a server that never answers keepalives.
func newSilentServer() *testServer {
	return newConfiguredServer(true, nil)
}

// newSignalServer returns a server that reports each signal it receives on
// signals.
func newSignalServer(signals chan string) *testServer {
	return newConfiguredServer(false, signals)
}

func newConfiguredServer(silent bool, signals chan string) *testServer {
	return &testServer{
		sessions:    atomic.Int32{},
		ptySize:     atomic.Value{},
		envRequests: atomic.Int32{},
		signals:     signals,
		silent:      silent,
	}
}

func (s *testServer) serve(nc net.Conn, config *ssh.ServerConfig) {
	sshConn, chans, globalReqs, err := ssh.NewServerConn(nc, config)
	if err != nil {
		return
	}

	defer func() { _ = sshConn.Close() }()

	go func() {
		for req := range globalReqs {
			if !s.silent {
				reply(req, false)
			}
		}
	}()

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "")

			continue
		}

		channel, sessionReqs, err := newChannel.Accept()
		if err != nil {
			return
		}

		go s.session(channel, sessionReqs)
	}
}

func reply(req *ssh.Request, ok bool) {
	_ = req.Reply(ok, nil)
}

// unmarshal decodes an SSH request payload. A malformed one leaves out
// zero, which the tests' own assertions would catch.
func unmarshal(payload []byte, out any) {
	_ = ssh.Unmarshal(payload, out)
}

func (s *testServer) session(channel ssh.Channel, reqs <-chan *ssh.Request) {
	s.sessions.Add(1)

	defer func() { _ = channel.Close() }()

	signal := make(chan string, 1)

	closed := make(chan struct{})
	defer close(closed)

	hasPty := false

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			hasPty = true

			var payload struct {
				Term                         string
				Columns, Rows, Width, Height uint32
				Modes                        string
			}

			unmarshal(req.Payload, &payload)
			s.ptySize.Store(fmt.Sprintf("%dx%d", payload.Columns, payload.Rows))
			reply(req, true)
		case "env":
			s.envRequests.Add(1)
			reply(req, false)
		case "signal":
			var payload struct{ Signal string }

			unmarshal(req.Payload, &payload)

			if s.signals != nil {
				s.signals <- payload.Signal
			}

			select {
			case signal <- payload.Signal:
			default:
			}
		case "exec":
			var payload struct{ Command string }

			unmarshal(req.Payload, &payload)
			reply(req, true)

			go run(channel, argvOf(payload.Command), hasPty, signal, closed)
		default:
			reply(req, false)
		}
	}
}

func run(channel ssh.Channel, argv []string, hasPty bool, signal <-chan string, closed <-chan struct{}) {
	switch argv[0] {
	case behaviorExit:
		status, _ := strconv.ParseUint(argv[1], 10, 32)
		exitStatus(channel, uint32(status))
	case "kill":
		exitSignal(channel, "KILL")
	case "drop":
		_ = channel.Close()
	case behaviorCat:
		_, _ = io.Copy(channel, channel)
		exitStatus(channel, 0)
	case behaviorPty:
		reportPty(channel, hasPty)
	case "sleep":
		exitSignal(channel, <-signal)
	case behaviorHang:
		<-closed
	case "flood":
		flood(channel)
	}
}

func reportPty(channel ssh.Channel, hasPty bool) {
	if hasPty {
		_, _ = io.WriteString(channel, behaviorPty)
	} else {
		_, _ = io.WriteString(channel, "nopty")
	}

	exitStatus(channel, 0)
}

// flood writes output until the channel fails.
func flood(channel ssh.Channel) {
	for {
		_, err := io.WriteString(channel, "x")
		if err != nil {
			return
		}

		time.Sleep(time.Millisecond)
	}
}

func exitStatus(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = channel.Close()
}

func exitSignal(channel ssh.Channel, name string) {
	_, _ = channel.SendRequest("exit-signal", false, ssh.Marshal(struct {
		Signal     string
		CoreDumped bool
		Error      string
		Lang       string
	}{Signal: name, CoreDumped: false, Error: "", Lang: ""}))
	_ = channel.Close()
}

// argvOf recovers argv from a CommandLine string: it unquotes the words and
// drops sh, -c, the wrapper, $0, the directory, and any env words.
func argvOf(cmd string) []string {
	var (
		words   []string
		current strings.Builder
	)

	for pos := 0; pos < len(cmd); {
		switch {
		case strings.HasPrefix(cmd[pos:], `'\''`):
			current.WriteByte('\'')

			pos += 4
		case cmd[pos] == '\'':
			end := strings.IndexByte(cmd[pos+1:], '\'')
			current.WriteString(cmd[pos+1 : pos+1+end])
			pos += end + 2
		case cmd[pos] == ' ':
			words = append(words, current.String())
			current.Reset()

			pos++
		default:
			panic("unquoted byte in command: " + cmd)
		}
	}

	words = append(words, current.String())[5:]
	if words[0] == "env" {
		words = words[1:]
		for strings.Contains(words[0], "=") {
			words = words[1:]
		}
	}

	return words
}

// testSigner is a client key for one test. It wraps the signer so helpers
// can return a concrete type.
type testSigner struct{ ssh.Signer }

func newTestSigner(t *testing.T) testSigner {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	return testSigner{Signer: signer}
}

// serverConfig accepts only signer.
func serverConfig(t *testing.T, signer testSigner) *ssh.ServerConfig {
	t.Helper()

	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Rand:           nil,
			RekeyThreshold: 0,
			KeyExchanges:   nil,
			Ciphers:        nil,
			MACs:           nil,
		},
		PublicKeyAuthAlgorithms:     nil,
		NoClientAuth:                false,
		NoClientAuthCallback:        nil,
		MaxAuthTries:                0,
		PasswordCallback:            nil,
		VerifiedPublicKeyCallback:   nil,
		KeyboardInteractiveCallback: nil,
		AuthLogCallback:             nil,
		PreAuthConnCallback:         nil,
		ServerVersion:               "",
		BannerCallback:              nil,
		GSSAPIWithMICConfig:         nil,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
				return nil, errUnknownKey
			}

			return new(ssh.Permissions), nil
		},
	}

	hostKey := newTestSigner(t)
	config.AddHostKey(hostKey)

	return config
}

// dialTest returns a Conn to the server over an in-memory pipe.
func dialTest(t *testing.T, server *testServer) *remote.Conn {
	t.Helper()

	return dialTestTiming(t, server, remote.DefaultTiming())
}

// dialTestTiming is dialTest with the given intervals.
func dialTestTiming(t *testing.T, server *testServer, timing remote.Timing) *remote.Conn {
	t.Helper()

	signer := newTestSigner(t)
	client, serverEnd := relayPipe()

	go server.serve(serverEnd, serverConfig(t, signer))

	return dialOver(t, client, signer, timing)
}

// dialOver runs the SSH handshake over client and closes the Conn when the
// test ends.
func dialOver(t *testing.T, client net.Conn, signer testSigner, timing remote.Timing) *remote.Conn {
	t.Helper()

	conn, err := remote.NewConn(context.Background(), client, "codespace", signer, timing)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// testCmd returns a command that runs argv in workDir.
func testCmd(argv ...string) remote.Cmd {
	return remote.Cmd{Dir: workDir, Env: nil, Argv: argv, Stdin: nil, Stdout: nil, Stderr: nil, TTY: nil}
}

func execArgs(ctx context.Context, t *testing.T, conn *remote.Conn, argv ...string) (int, string, error) {
	t.Helper()

	var out bytes.Buffer

	cmd := testCmd(argv...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard

	code, err := conn.Exec(ctx, cmd)

	return code, out.String(), err
}

// waitForSessions blocks until server has opened at least want sessions.
func waitForSessions(t *testing.T, server *testServer, want int32) {
	t.Helper()

	deadline := time.Now().Add(settleTimeout)
	for server.sessions.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("server opened %d sessions, want %d", server.sessions.Load(), want)
		}

		time.Sleep(time.Millisecond)
	}
}

// Exit statuses, exit signals, and a missing exit status.
func TestExitMapping(t *testing.T) {
	t.Parallel()

	conn := dialTest(t, newTestServer())
	ctx := context.Background()

	tests := []struct {
		argv    []string
		code    int
		wantErr error
	}{
		{[]string{behaviorExit, "0"}, 0, nil},
		{[]string{behaviorExit, "7"}, 7, nil},
		{[]string{"kill"}, 137, nil},
		{[]string{"drop"}, 0, remote.ErrNoExitStatus},
	}
	for _, testCase := range tests {
		code, _, err := execArgs(ctx, t, conn, testCase.argv...)
		if code != testCase.code || !errors.Is(err, testCase.wantErr) {
			t.Errorf("%v: got %d, %v; want %d, %v", testCase.argv, code, err, testCase.code, testCase.wantErr)
		}
	}
}

// Cancellation sends SIGTERM, and the Conn stays usable, including
// after a command that ignores the signal.
func TestCancelSignalsAndConnSurvives(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.SignalGrace = 100 * time.Millisecond

	server := newSignalServer(make(chan string, 4))
	conn := dialTestTiming(t, server, timing)

	for _, behavior := range []string{"sleep", behaviorHang} {
		ctx, cancel := context.WithCancelCause(context.Background())
		done := make(chan error, 1)
		opened := server.sessions.Load()

		go func() {
			_, _, err := execArgs(ctx, t, conn, behavior)
			done <- err
		}()

		// Cancelling before the session opens would skip the command, and
		// its signal.
		waitForSessions(t, server, opened+1)
		cancel(errStop)

		if got := <-server.signals; got != "TERM" {
			t.Errorf("%s: signal = %q, want TERM", behavior, got)
		}

		err := <-done
		if !errors.Is(err, errStop) {
			t.Errorf("%s: Exec error = %v, want the context cause", behavior, err)
		}

		code, _, err := execArgs(context.Background(), t, conn, behaviorExit, "3")
		if code != 3 || err != nil {
			t.Errorf("after %s: got %d, %v; want 3", behavior, code, err)
		}
	}
}

// Stdin is EOF unless attached, no pty without a TTY, and no
// env requests are sent.
func TestStdinAndPty(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	conn := dialTest(t, server)
	ctx := context.Background()

	code, out, err := execArgs(ctx, t, conn, behaviorCat)
	if code != 0 || out != "" || err != nil {
		t.Errorf("cat without stdin: got %d, %q, %v", code, out, err)
	}

	var catOut bytes.Buffer

	cat := testCmd(behaviorCat)
	cat.Stdin = strings.NewReader("hello")
	cat.Stdout = &catOut

	code, err = conn.Exec(ctx, cat)
	if code != 0 || catOut.String() != "hello" || err != nil {
		t.Errorf("cat with stdin: got %d, %q, %v", code, catOut.String(), err)
	}

	_, out, _ = execArgs(ctx, t, conn, behaviorPty)
	if out != "nopty" {
		t.Errorf("pty without TTY: %q", out)
	}

	withEnv := testCmd(behaviorExit, "0")
	withEnv.Env = []string{"A=1"}

	_, err = conn.Exec(ctx, withEnv)
	if err != nil {
		t.Fatal(err)
	}

	if count := server.envRequests.Load(); count != 0 {
		t.Errorf("%d env requests sent", count)
	}
}

// An unanswered keepalive closes the Conn.
func TestKeepaliveTimeoutClosesConn(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.KeepaliveEvery = 20 * time.Millisecond
	timing.KeepaliveTimeout = 20 * time.Millisecond

	conn := dialTestTiming(t, newSilentServer(), timing)
	select {
	case <-conn.Done():
	case <-time.After(settleTimeout):
		t.Fatal("Conn still open after an unanswered keepalive")
	}

	_, _, err := execArgs(context.Background(), t, conn, behaviorExit, "0")
	if err == nil {
		t.Error("Exec succeeded on a closed Conn")
	}
}

// An answered keepalive keeps the Conn open.
func TestKeepaliveAnswered(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.KeepaliveEvery = 10 * time.Millisecond
	timing.KeepaliveTimeout = 200 * time.Millisecond

	conn := dialTestTiming(t, newTestServer(), timing)

	// Several timeouts long, so an unanswered keepalive would close conn.
	time.Sleep(time.Second)

	select {
	case <-conn.Done():
		t.Fatal("keepalives closed a Conn whose peer answers them")
	default:
	}

	code, _, err := execArgs(context.Background(), t, conn, behaviorExit, "4")
	if code != 4 || err != nil {
		t.Errorf("got %d, %v; want 4", code, err)
	}
}

// A keepalive that fails because the peer is gone closes the Conn.
func TestKeepaliveErrorClosesConn(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.KeepaliveEvery = 10 * time.Millisecond

	signer := newTestSigner(t)
	client, server := relayPipe()

	go newTestServer().serve(server, serverConfig(t, signer))

	conn := dialOver(t, client, signer, timing)

	_ = server.Close()

	select {
	case <-conn.Done():
	case <-time.After(settleTimeout):
		t.Fatal("Conn still open after its peer went away")
	}
}

// Cancelling the dial context aborts a stalled handshake at once.
func TestHandshakeCancelled(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()

	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())

	time.AfterFunc(20*time.Millisecond, func() { cancel(errStop) })

	start := time.Now()

	_, err := remote.NewConn(ctx, client, "u", newTestSigner(t), remote.DefaultTiming())
	if !errors.Is(err, errStop) {
		t.Errorf("err = %v, want the context cause", err)
	}

	if elapsed := time.Since(start); elapsed > settleTimeout {
		t.Errorf("took %v", elapsed)
	}
}

// A cancelled context never starts the command.
func TestExecCancelledBeforeStart(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	conn := dialTest(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := execArgs(ctx, t, conn, behaviorExit, "0")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}

	if count := server.sessions.Load(); count != 0 {
		t.Errorf("%d sessions opened", count)
	}
}

// A stalled handshake fails within its deadline and runs the teardown.
func TestHandshakeDeadline(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.Handshake = 50 * time.Millisecond

	client, server := net.Pipe()

	defer func() { _ = server.Close() }()

	tornDown := false
	start := time.Now()

	_, err := remote.NewConn(context.Background(), client, "u", newTestSigner(t), timing, func() error {
		tornDown = true

		return nil
	})
	if err == nil {
		t.Fatal("handshake succeeded against a silent peer")
	}

	if elapsed := time.Since(start); elapsed > settleTimeout {
		t.Errorf("handshake took %v", elapsed)
	}

	if !tornDown {
		t.Error("teardown did not run")
	}
}

// Many sequential and concurrent sessions share one Conn.
func TestManySessions(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	conn := dialTest(t, server)
	ctx := context.Background()

	for i := range 10 {
		code, _, err := execArgs(ctx, t, conn, behaviorExit, strconv.Itoa(i))
		if code != i || err != nil {
			t.Fatalf("sequential %d: got %d, %v", i, code, err)
		}
	}

	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			code, _, err := execArgs(ctx, t, conn, behaviorExit, strconv.Itoa(i))
			if code != i || err != nil {
				t.Errorf("concurrent %d: got %d, %v", i, code, err)
			}
		})
	}

	group.Wait()

	if count := server.sessions.Load(); count != 30 {
		t.Errorf("sessions = %d, want 30", count)
	}
}

func TestCommandLineQuotesEveryWord(t *testing.T) {
	t.Parallel()

	got := remote.CommandLine("/w s", []string{"A=it's"}, []string{"printf", "%s", "-x"})

	want := `'sh' '-c' '` + remote.Wrapper + `' 'runspace' '/w s' 'env' 'A=it'\''s' 'printf' '%s' '-x'`
	if got != want {
		t.Errorf("CommandLine =\n%s\nwant\n%s", got, want)
	}
}

// relayPipe returns two connected in-memory conns with a copying relay
// between them, as the port forwarder provides in production. A bare
// net.Pipe deadlocks the SSH handshake, because both peers write their
// version banner before reading.
func relayPipe() (net.Conn, net.Conn) {
	client, clientEnd := net.Pipe()
	serverEnd, server := net.Pipe()

	go func() {
		_, _ = io.Copy(clientEnd, serverEnd)
		_ = clientEnd.Close()
	}()
	go func() {
		_, _ = io.Copy(serverEnd, clientEnd)
		_ = serverEnd.Close()
	}()

	return client, server
}

// A failure of the caller's own stream is a StreamError, not a transport
// error, even when the remote command exits 0.
func TestStreamError(t *testing.T) {
	t.Parallel()

	conn := dialTest(t, newTestServer())

	cmd := testCmd(behaviorPty)
	cmd.Stdout = failingWriter{err: errDiskFull}

	_, err := conn.Exec(context.Background(), cmd)

	var streamErr *remote.StreamError
	if !errors.As(err, &streamErr) || !errors.Is(err, errDiskFull) {
		t.Fatalf("err = %v, want a StreamError wrapping the writer's error", err)
	}

	code, _, err := execArgs(context.Background(), t, conn, behaviorExit, "0")
	if code != 0 || err != nil {
		t.Errorf("Conn unusable after a stream error: %d, %v", code, err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// When the tunnel stalls toward the peer, the signal and the close never
// arrive, but the peer's output still does. Once Exec has returned, that
// output must not reach the caller's buffers.
func TestStalledPeerDetached(t *testing.T) {
	t.Parallel()

	timing := remote.DefaultTiming()
	timing.SignalGrace = 30 * time.Millisecond
	timing.CloseWait = 30 * time.Millisecond

	signer := newTestSigner(t)
	client, server, pause := pausableRelay()

	go newTestServer().serve(server, serverConfig(t, signer))

	conn := dialOver(t, client, signer, timing)

	var out lockedBuffer

	ctx, cancel := context.WithCancel(context.Background())

	time.AfterFunc(30*time.Millisecond, func() {
		pause()
		cancel()
	})

	cmd := testCmd("flood")
	cmd.Stdout = &out

	_, err := conn.Exec(ctx, cmd)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	length := out.Len()

	time.Sleep(100 * time.Millisecond)

	if out.Len() != length {
		t.Errorf("output grew from %d to %d bytes after Exec returned", length, out.Len())
	}
}

// pausableRelay is relayPipe with a switch that stops the client-to-server
// direction, as a stalled tunnel would. It returns the two ends and the
// switch.
func pausableRelay() (net.Conn, net.Conn, func()) {
	client, clientEnd := net.Pipe()
	serverEnd, server := net.Pipe()
	stop := make(chan struct{})

	var once sync.Once

	go func() {
		_, _ = io.Copy(clientEnd, serverEnd)
		_ = clientEnd.Close()
	}()
	go relayUntilPaused(clientEnd, serverEnd, stop)

	return client, server, func() { once.Do(func() { close(stop) }) }
}

// relayUntilPaused copies from src to dst until src ends, and swallows
// everything once stop is closed.
func relayUntilPaused(src, dst net.Conn, stop <-chan struct{}) {
	buf := make([]byte, 32<<10)

	for {
		count, err := src.Read(buf)

		select {
		case <-stop:
			// Swallow everything from now on.
			if err != nil {
				return
			}

			continue
		default:
		}

		if count > 0 {
			_, _ = dst.Write(buf[:count])
		}

		if err != nil {
			_ = dst.Close()

			return
		}
	}
}

// lockedBuffer is a bytes.Buffer that is safe for a writer and a reader.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written, err := b.buf.Write(data)
	if err != nil {
		return written, fmt.Errorf("write to buffer: %w", err)
	}

	return written, nil
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Len()
}
