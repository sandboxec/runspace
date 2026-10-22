// Package sandbox defines runspace's seven commands once, for both the CLI
// and the MCP server. Each command has one options struct and one method on
// Sandboxes; every options struct but ListOptions, which has no options, has
// one Validate.
//
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"charm.land/log/v2"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
)

// scriptName is $0 of the scripts that run through sh -c.
const scriptName = "runspace"

// scriptArgv is the command that runs script, one of runspace's own, with
// args as $1, $2, and so on.
func scriptArgv(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, scriptName}, args...)
}

const (
	// idleClose closes a cached Conn after this long without a call.
	idleClose = 10 * time.Minute
	// dialTimeout bounds a cached dial, which outlives the call that
	// started it. Dialing a stopped codespace includes starting it.
	dialTimeout = 10 * time.Minute
	// finishTimeout bounds the stop or delete that ends create's cleanup and
	// run.
	finishTimeout = 2 * time.Minute
)

var errNotTagged = errors.New("was not created by runspace")

// Conn is one connection to a sandbox. *remote.Conn implements it.
type Conn interface {
	Exec(ctx context.Context, cmd remote.Cmd) (int, error)
	// Done is closed once the Conn is closed, including by a failed
	// keepalive.
	Done() <-chan struct{}
	// Close may be called more than once, as calls that share a cached Conn
	// can each close it after a transport error.
	Close() error
}

// DialFunc opens a Conn to the named sandbox. progress receives a line for
// each slow phase.
type DialFunc func(ctx context.Context, name string, progress func(string)) (Conn, error)

// Config configures Sandboxes.
type Config struct {
	// Token, unless "", is the GitHub token, and RUNSPACE_TOKEN is not read.
	Token string
	// Getenv reads RUNSPACE_TOKEN. Nil means os.Getenv.
	Getenv func(string) string
	// APIClient, when non-nil, replaces the GitHub API client built from the
	// token. Tests use it.
	APIClient *http.Client
	// Dial, when non-nil, replaces remote.Dial. Tests use it.
	Dial DialFunc
	// Logger receives debug detail, such as each dial and each command's
	// exit code, but never a token, an environment value, or a command
	// line. Nil discards it.
	Logger *log.Logger
	// Reuse keeps one Conn per sandbox across calls and closes it after
	// idleClose without a call, as the MCP server needs. Otherwise each
	// command dials and closes, as the CLI needs.
	Reuse bool
}

// Streams are the caller's streams. They are not options: each front end
// supplies its own.
type Streams struct {
	// Stdin is attached to the remote command when non-nil.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// TTY is the local terminal for exec -t.
	TTY *os.File
	// Progress receives a line for each slow phase, such as provisioning.
	Progress func(string)
}

func (s Streams) progress() func(string) { return orNoProgress(s.Progress) }

// orNoProgress returns progress, or a function that drops every line when
// progress is nil.
func orNoProgress(progress func(string)) func(string) {
	if progress == nil {
		return func(string) {}
	}

	return progress
}

// Sandboxes runs the commands. It is safe for concurrent use.
type Sandboxes struct {
	api    *github.Client
	dial   DialFunc
	logger *log.Logger
	reuse  bool

	mu    sync.Mutex
	conns map[string]*cached
}

// New returns Sandboxes that use cfg.Token, or else RUNSPACE_TOKEN from
// cfg.Getenv, as the GitHub token.
func New(cfg Config) (*Sandboxes, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard)
	}

	apiClient, tunnelClient := cfg.APIClient, new(http.Client)
	if apiClient == nil {
		token, err := github.Token(cfg.Token, getenv)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}

		apiClient, tunnelClient, err = github.NewClients(token, github.DefaultAPIURL)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}

	dial := cfg.Dial
	if dial == nil {
		dial = remoteDialer(apiClient, tunnelClient, logger)
	}

	return &Sandboxes{
		api:    github.NewClient(apiClient, github.DefaultAPIURL),
		dial:   logDials(dial, logger),
		logger: logger,
		reuse:  cfg.Reuse,
		mu:     sync.Mutex{},
		conns:  make(map[string]*cached),
	}, nil
}

// remoteDialer returns the DialFunc that connects with remote.Dial.
func remoteDialer(apiClient, tunnelClient *http.Client, logger *log.Logger) DialFunc {
	return func(ctx context.Context, name string, progress func(string)) (Conn, error) {
		conn, err := remote.Dial(ctx, remote.DialConfig{
			Name:         name,
			APIClient:    apiClient,
			TunnelClient: tunnelClient,
			Progress:     progress,
			Logger:       logger,
		})
		if errors.Is(err, remote.ErrTokenRejected) {
			return nil, fmt.Errorf("%w (%s)", err, github.TokenHint)
		}

		if err != nil {
			// Not conn: a nil *remote.Conn in a Conn is not a nil Conn.
			return nil, fmt.Errorf("%w", err)
		}

		return conn, nil
	}
}

// logDials returns dial, logging each dial and how it ended.
func logDials(dial DialFunc, logger *log.Logger) DialFunc {
	return func(ctx context.Context, name string, progress func(string)) (Conn, error) {
		logger.Debug("Dialing", "name", name)

		start := time.Now()

		conn, err := dial(ctx, name, progress)
		if err != nil {
			logger.Debug("Dial failed", "name", name, "err", err)

			return nil, err
		}

		logger.Debug("Connected", "name", name, "took", time.Since(start).Round(time.Millisecond))

		return conn, nil
	}
}

// Close closes every cached Conn and leaves every sandbox as it is. It also
// cancels the dials still in flight.
func (s *Sandboxes) Close() error {
	s.mu.Lock()
	conns := s.conns
	s.conns = make(map[string]*cached)
	s.mu.Unlock()

	// Cancel every running dial before waiting for any, so they unwind
	// together.
	for _, entry := range conns {
		if entry.cancel != nil {
			entry.cancel()
		}
	}

	errs := make([]error, 0, len(conns))
	for _, entry := range conns {
		errs = append(errs, entry.close())
	}

	return errors.Join(errs...)
}

// isRunspace reports whether runspace created codespace. Every command that
// takes a name, ls, and delete --all act only on such codespaces.
func isRunspace(codespace *github.Codespace) bool {
	return codespace.DisplayName == DisplayName
}

// tagged returns the named codespace if runspace created it.
func (s *Sandboxes) tagged(ctx context.Context, name string) (*github.Codespace, error) {
	codespace, err := s.api.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if !isRunspace(codespace) {
		return nil, fmt.Errorf("codespace %s %w (its display name is %q, not %q)",
			name, errNotTagged, codespace.DisplayName, DisplayName)
	}

	return codespace, nil
}

// session is a Conn to a sandbox and the path of its workspace.
type session struct {
	conn      Conn
	workspace string
}

// closing returns a lease on sess that closes its Conn when released.
func (sess session) closing() *lease {
	return &lease{
		session: sess,
		release: func(error) { _ = sess.conn.Close() },
	}
}

// lease is a session held for one call. run, finish, or release must be
// called once.
type lease struct {
	session

	release func(transportErr error)
}

// finish releases l after a remote command that ran under ctx and returned
// err. Remote commands are never retried.
func (l *lease) finish(ctx context.Context, err error) {
	l.release(transportFailure(ctx, err))
}

// run runs cmd over l's Conn under the timeout in opts, then releases l.
// The timeout bounds only the command, not the dial, and l is released
// judged against the command's own context, so a timeout or an interrupt
// does not drop a healthy Conn. explain words a timeout error with what.
func (l *lease) run(ctx context.Context, opts *TimeoutOption, what string, cmd remote.Cmd) (int, error) {
	cmdCtx, cancel := opts.deadline(ctx)
	defer cancel()

	code, err := l.conn.Exec(cmdCtx, cmd)
	l.finish(cmdCtx, err)

	return code, opts.explain(what, err)
}

// transportFailure returns err when it means the Conn is broken: an error
// while ctx was still live, other than a failure of the caller's own
// streams. Otherwise it returns nil.
func transportFailure(ctx context.Context, err error) error {
	var local *remote.StreamError
	if err != nil && ctx.Err() == nil && !errors.As(err, &local) {
		return err
	}

	return nil
}

// acquire returns a lease on a Conn to a runspace-tagged sandbox.
func (s *Sandboxes) acquire(ctx context.Context, name string, progress func(string)) (*lease, error) {
	if !s.reuse {
		sess, err := s.open(ctx, name, progress)
		if err != nil {
			return nil, err
		}

		return sess.closing(), nil
	}

	entry, reused := s.join(ctx, name, progress)
	if reused {
		s.logger.Debug("Reusing the connection", "name", name)
	}

	select {
	case <-entry.ready:
	case <-ctx.Done():
		s.release(name, entry, nil)

		return nil, fmt.Errorf("%w", context.Cause(ctx))
	}

	if entry.err != nil {
		s.release(name, entry, entry.err)

		return nil, entry.err
	}

	return &lease{
		session: entry.session,
		release: func(err error) { s.release(name, entry, err) },
	}, nil
}

// join counts one call on the cached entry of name, and starts the dial if
// there is no usable entry. It reports whether the entry was cached already.
func (s *Sandboxes) join(ctx context.Context, name string, progress func(string)) (*cached, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.conns[name]
	if entry != nil && entry.dead() {
		// The keepalive closed it, say after a network change. The next
		// command has not started, so dialing again is not a retry.
		delete(s.conns, name)

		entry = nil
	}

	reused := entry != nil
	if !reused {
		entry = s.startDial(ctx, name, progress)
	}

	entry.inflight++
	if entry.idle != nil {
		entry.idle.Stop()
		entry.idle = nil
	}

	return entry, reused
}

// startDial caches a new entry for name and dials in the background. The
// caller holds s.mu.
func (s *Sandboxes) startDial(ctx context.Context, name string, progress func(string)) *cached {
	// The dial is shared, so it must not end with this call's context.
	dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dialTimeout)
	entry := &cached{
		ready:    make(chan struct{}),
		cancel:   cancel,
		session:  session{conn: nil, workspace: ""},
		err:      nil,
		inflight: 0,
		idle:     nil,
	}
	s.conns[name] = entry

	go s.finishDial(dialCtx, name, entry, progress)

	return entry
}

// finishDial runs the dial of entry and publishes its outcome.
func (s *Sandboxes) finishDial(ctx context.Context, name string, entry *cached, progress func(string)) {
	defer entry.cancel()

	entry.session, entry.err = s.open(ctx, name, progress)
	close(entry.ready)

	if entry.err == nil {
		return
	}
	// Evict the failure, so a later call dials again even if every caller
	// gave up waiting.
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conns[name] == entry {
		delete(s.conns, name)
	}
}

// release ends one call on entry. A transport error or a failed dial evicts
// entry, so the next call dials again. The last call to finish starts the
// idle timer.
func (s *Sandboxes) release(name string, entry *cached, err error) {
	s.mu.Lock()
	entry.inflight--

	current := s.conns[name] == entry
	if err != nil {
		if current {
			delete(s.conns, name)
		}
		s.mu.Unlock()
		s.logger.Debug("Dropping the connection", "name", name, "err", err)
		entry.discard()

		return
	}

	if current && entry.inflight == 0 {
		entry.idle = time.AfterFunc(idleClose, func() { s.expire(name, entry) })
	}
	s.mu.Unlock()
}

// expire closes entry if it is still cached and idle.
func (s *Sandboxes) expire(name string, entry *cached) {
	s.mu.Lock()
	if s.conns[name] != entry || entry.inflight > 0 {
		s.mu.Unlock()

		return
	}

	delete(s.conns, name)
	s.mu.Unlock()
	s.logger.Debug("Closing the idle connection", "name", name)
	entry.discard()
}

// forget closes and evicts the cached Conn of name, if any.
func (s *Sandboxes) forget(name string) {
	s.mu.Lock()
	entry := s.conns[name]
	delete(s.conns, name)

	if entry != nil && entry.idle != nil {
		entry.idle.Stop()
	}
	s.mu.Unlock()

	if entry != nil {
		entry.discard()
	}
}

// adopt caches sess, which create already dialed, or closes it when Conns
// are not reused.
func (s *Sandboxes) adopt(name string, sess session) {
	if !s.reuse {
		_ = sess.conn.Close()

		return
	}

	entry := &cached{
		ready:    make(chan struct{}),
		cancel:   nil,
		session:  sess,
		err:      nil,
		inflight: 0,
		idle:     nil,
	}
	close(entry.ready)
	s.mu.Lock()
	old := s.conns[name]
	s.conns[name] = entry
	entry.idle = time.AfterFunc(idleClose, func() { s.expire(name, entry) })
	s.mu.Unlock()

	if old != nil {
		old.discard()
	}
}

// open checks the tag and dials.
func (s *Sandboxes) open(ctx context.Context, name string, progress func(string)) (session, error) {
	codespace, err := s.tagged(ctx, name)
	if err != nil {
		return session{}, err
	}

	conn, err := s.dial(ctx, name, progress)
	if err != nil {
		return session{}, err
	}

	return session{conn: conn, workspace: workspacePath(codespace.Repository.Name)}, nil
}

func workspacePath(repo string) string {
	return "/workspaces/" + repo
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	return remote.IsTerminal(f)
}
