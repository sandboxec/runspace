// Package remote runs commands in a codespace over one in-process SSH
// connection.
//
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Defaults for the intervals that govern a Conn.
const (
	defaultHandshakeTimeout = 30 * time.Second
	defaultKeepaliveEvery   = 60 * time.Second
	defaultKeepaliveTimeout = 30 * time.Second
	// defaultSignalGrace is how long a cancelled command has to exit after
	// SIGTERM before its session is closed.
	defaultSignalGrace = 10 * time.Second
	// defaultCloseWait is how long Exec waits for the peer to confirm a
	// session close.
	defaultCloseWait = 2 * time.Second
)

// timing holds the intervals of one Conn. Tests shorten them per Conn.
type timing struct {
	Handshake        time.Duration
	KeepaliveEvery   time.Duration
	KeepaliveTimeout time.Duration
	SignalGrace      time.Duration
	CloseWait        time.Duration
}

func defaultTiming() timing {
	return timing{
		Handshake:        defaultHandshakeTimeout,
		KeepaliveEvery:   defaultKeepaliveEvery,
		KeepaliveTimeout: defaultKeepaliveTimeout,
		SignalGrace:      defaultSignalGrace,
		CloseWait:        defaultCloseWait,
	}
}

var (
	// ErrNoExitStatus means the session ended without reporting how the
	// remote command exited.
	ErrNoExitStatus = errors.New("connection closed before exit status")

	errEmptyCommand = errors.New("empty command")
)

// StreamError is a failure of one of the caller's own streams, such as a
// full disk under Stdout or a failed reader under Stdin. The Conn is still
// healthy, but the command's result is incomplete.
type StreamError struct{ Err error }

func (e *StreamError) Error() string { return e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

// Cmd is one remote command.
type Cmd struct {
	// Dir is the absolute remote working directory.
	Dir string
	// Env holds KEY=VALUE pairs. The command receives them as env words.
	Env []string
	// Argv is the command and its arguments. It must not be empty.
	Argv []string

	// Stdin is attached when non-nil. Otherwise the command reads EOF.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// TTY, when non-nil, is the local terminal. The session gets a pty of
	// the terminal's size, and the terminal is in raw mode while the command
	// runs.
	TTY *os.File
}

// Conn is one SSH connection to a codespace. It is safe for concurrent use,
// and each Exec opens its own session.
type Conn struct {
	client *ssh.Client

	// teardown undoes the dial steps that precede the SSH handshake, in
	// reverse order of setup.
	teardown []func() error

	timing timing

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

// newConn performs the SSH handshake over tunnel and starts the keepalive.
// It takes ownership of tunnel and of teardown, which run on Close, or at once when
// the handshake fails. Cancelling ctx aborts the handshake.
func newConn(
	ctx context.Context,
	tunnel net.Conn,
	user string,
	signer ssh.Signer,
	intervals timing,
	teardown ...func() error,
) (*Conn, error) {
	teardown = append(teardown, tunnel.Close)

	client, err := handshake(ctx, tunnel, user, signer, intervals.Handshake)
	if err != nil {
		_ = runTeardown(teardown)

		return nil, err
	}

	conn := &Conn{
		client:    client,
		teardown:  teardown,
		timing:    intervals,
		closeOnce: sync.Once{},
		closeErr:  nil,
		done:      make(chan struct{}),
	}
	go conn.keepalive()

	return conn, nil
}

// acceptAnyHostKey accepts every host key. The tunnel authenticates the
// endpoint, and the codespace controls its own sshd host key anyway.
func acceptAnyHostKey(string, net.Addr, ssh.PublicKey) error { return nil }

// handshake runs the SSH handshake over tunnel within timeout. Cancelling
// ctx aborts it. On failure it may have closed tunnel, as closing the SSH
// connection does; newConn's teardown closes it again, and runTeardown
// ignores the already-closed error.
func handshake(
	ctx context.Context,
	tunnel net.Conn,
	user string,
	signer ssh.Signer,
	timeout time.Duration,
) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		Config: ssh.Config{
			Rand:           nil,
			RekeyThreshold: 0,
			KeyExchanges:   nil,
			Ciphers:        nil,
			MACs:           nil,
		},
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   acceptAnyHostKey,
		BannerCallback:    nil,
		ClientVersion:     "",
		HostKeyAlgorithms: nil,
		Timeout:           0,
		AuthCallback:      nil,
	}

	// The deadline bounds only the handshake. Cancelling ctx moves it to now.
	err := tunnel.SetDeadline(time.Now().Add(timeout))
	if err != nil {
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	stop := context.AfterFunc(ctx, func() { _ = tunnel.SetDeadline(time.Now()) })

	sshConn, chans, reqs, err := ssh.NewClientConn(tunnel, "codespace", config)
	if !stop() && err == nil {
		err = cause(ctx)
	}

	if err != nil {
		if sshConn != nil {
			_ = sshConn.Close()
		}

		if ctx.Err() != nil {
			return nil, cause(ctx)
		}

		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	err = tunnel.SetDeadline(time.Time{})
	if err != nil {
		_ = sshConn.Close()

		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	// ssh.NewClient rejects channel types without a handler, so agent, X11,
	// and forwarded-port channels are refused.
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// cause returns why ctx is done. The wrap adds no text, so the message is
// the cause's own.
func cause(ctx context.Context) error {
	return fmt.Errorf("%w", context.Cause(ctx))
}

// Done returns a channel that is closed once the Conn is closed, by Close
// or by a failed keepalive.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close closes the SSH connection and then tears down the tunnel. It is safe
// to call more than once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)

		err := c.client.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}

		c.closeErr = errors.Join(err, runTeardown(c.teardown))
	})

	return c.closeErr
}

func runTeardown(fns []func() error) error {
	var errs []error

	for _, fn := range slices.Backward(fns) {
		err := fn()
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Exec runs cmd in a new session and returns the remote exit code. A command
// killed by signal n reports 128+n.
//
// When ctx is done, Exec sends SIGTERM to the remote command, waits up to
// the signal grace for it to exit, closes the session, and returns
// context.Cause(ctx).
// The Conn stays usable.
func (c *Conn) Exec(ctx context.Context, cmd Cmd) (int, error) {
	if len(cmd.Argv) == 0 {
		return 0, errEmptyCommand
	}
	// A cancelled call must not start its command at all.
	if ctx.Err() != nil {
		return 0, cause(ctx)
	}

	sess, err := c.client.NewSession()
	if err != nil {
		return 0, fmt.Errorf("open session: %w", err)
	}

	defer func() { _ = sess.Close() }()

	// The caller's streams are watched, so their failures are not mistaken
	// for the transport's, and detached when Exec returns.
	var local streams
	defer local.detach()

	local.attach(sess, cmd)

	restore, err := attachTTY(sess, cmd.TTY)
	if err != nil {
		return 0, err
	}
	defer restore()

	err = sess.Start(CommandLine(cmd.Dir, cmd.Env, cmd.Argv))
	if err != nil {
		return 0, fmt.Errorf("start command: %w", err)
	}

	// ended is closed once the command ends, and waitErr then says how.
	var waitErr error

	ended := make(chan struct{})

	go func() {
		waitErr = sess.Wait()

		close(ended)
	}()

	select {
	case <-ended:
		// x/crypto reports an exit status ahead of a stream failure, but a
		// failed stream means the result is incomplete either way.
		lerr := local.failure()
		if lerr != nil {
			return 0, &StreamError{Err: lerr}
		}

		return exitCode(waitErr)
	case <-ctx.Done():
	}

	c.terminate(sess, ended)

	return 0, cause(ctx)
}

// attachTTY gives sess a pty when tty is non-nil. The returned function
// restores the terminal, and is safe to call when there is nothing to do.
func attachTTY(sess *ssh.Session, tty *os.File) (func(), error) {
	if tty == nil {
		return func() {}, nil
	}

	return startTTY(sess, tty)
}

// terminate stops a command whose context is done: it sends SIGTERM, waits
// up to the signal grace for ended to close, then closes the session.
func (c *Conn) terminate(sess *ssh.Session, ended <-chan struct{}) {
	// sshd delivers the signal to the session's process group.
	_ = sess.Signal(ssh.SIGTERM)

	grace := time.NewTimer(c.timing.SignalGrace)
	defer grace.Stop()

	select {
	case <-ended:
	case <-grace.C:
		// Close only asks the peer to close the channel. Wait for that
		// briefly, but never hang on a stalled or hostile peer: the Wait
		// goroutine ends when the Conn closes.
		_ = sess.Close()

		select {
		case <-ended:
		case <-time.After(c.timing.CloseWait):
		}
	}
}

// keepalive sends keepalive@openssh.com every KeepaliveEvery and closes the
// Conn when a reply takes longer than KeepaliveTimeout. The traffic also
// keeps the codespace's activity heartbeat alive.
func (c *Conn) keepalive() {
	ticker := time.NewTicker(c.timing.KeepaliveEvery)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}

		if !c.peerAlive() {
			_ = c.Close()

			return
		}
	}
}

// peerAlive sends one keepalive and reports whether the Conn should stay open: it
// is false when the request fails or the reply takes longer than
// KeepaliveTimeout, and true when the Conn is closed meanwhile.
func (c *Conn) peerAlive() bool {
	replied := make(chan error, 1)

	go func() {
		// Any reply, including a refusal, proves the peer is alive.
		_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
		replied <- err
	}()

	timer := time.NewTimer(c.timing.KeepaliveTimeout)
	defer timer.Stop()

	select {
	case err := <-replied:
		return err == nil
	case <-timer.C:
		return false
	case <-c.done:
		return true
	}
}

// exitCode maps the result of ssh.Session.Wait to a remote exit code.
func exitCode(err error) (int, error) {
	var (
		exitErr *ssh.ExitError
		missing *ssh.ExitMissingError
	)

	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		// x/crypto reports an exit signal as 128+n.
		return exitErr.ExitStatus(), nil
	case errors.As(err, &missing):
		return 0, ErrNoExitStatus
	default:
		return 0, err
	}
}

// wrapper runs as `sh -c wrapper runspace DIR [env K=V...] ARGV...`. It runs
// the command as a child, so sh supplies 126 and 127 and a death by signal n
// exits 128+n for every signal. The final exit keeps bash and busybox sh from
// replacing themselves with the command, which would report signal deaths by
// name, and x/crypto knows only some names.
const wrapper = `cd -- "$1" || exit 125
shift
"$@"
exit "$?"`

// CommandLine returns the string sent as the SSH exec request: sh, -c, the
// wrapper, and its $0 runspace, then dir, then env and the KEY=VALUE pairs
// when env is set, then argv. Every word is single-quoted, so the login shell
// sees only quoted words.
func CommandLine(dir string, env, argv []string) string {
	words := []string{"sh", "-c", wrapper, "runspace", dir}
	if len(env) > 0 {
		words = append(words, "env")
		words = append(words, env...)
	}

	words = append(words, argv...)

	var builder strings.Builder

	for i, word := range words {
		if i > 0 {
			builder.WriteByte(' ')
		}

		builder.WriteString(quote(word))
	}

	return builder.String()
}

// quote wraps s in single quotes. Each single quote inside s ends the quoted
// string, adds an escaped quote, and reopens it.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
