// Package sandboxtest provides in-memory fakes for testing code built on
// package sandbox: the five documented Codespaces REST endpoints, and a
// scriptable SSH connection. Neither touches the network, so tests can use
// testing/synctest.
//
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandboxtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
)

// HeadSHA is the commit the fake prepare step reports.
const HeadSHA = "0123456789abcdef0123456789abcdef01234567"

// Patch is what the fake diff step prints.
const Patch = "diff --git a/f b/f\n"

const (
	// codespacesSegment is the path segment all codespace endpoints share.
	codespacesSegment = "codespaces"
	// messageKey is the key of an API error body.
	messageKey = "message"

	stepPrepare = "prepare"
	stepDiff    = "diff"
)

// GitHub fakes the documented Codespaces endpoints. Use Client as the API
// client.
type GitHub struct {
	// States is the sequence of states a new codespace reports, one per
	// request; the last repeats. Nil means Available at once.
	States []string
	// Fail maps "METHOD /path" to a status code returned instead of the
	// normal response, such as "POST /user/codespaces/cs-1/stop": 500.
	Fail map[string]int
	// OnCreate, when set, runs while a create request is being served, for
	// tests that act in the middle of the POST. It runs under the fake's
	// lock, so it must not call back into the fake, such as Calls,
	// Codespace, or Add.
	OnCreate func()

	mu      sync.Mutex
	spaces  map[string]*space
	order   []string
	next    int
	calls   []string
	creates []map[string]any
}

type space struct {
	codespace github.Codespace
	states    []string
}

// Client returns an *http.Client served by g.
func (g *GitHub) Client() *http.Client {
	client := new(http.Client)
	client.Transport = g

	return client
}

// Add seeds a codespace, such as one runspace did not create.
func (g *GitHub) Add(codespace github.Codespace) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.add(codespace, nil)
}

// Calls returns every request so far, as "METHOD /path".
func (g *GitHub) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.calls)
}

// Called reports whether a request was made, as "METHOD /path".
func (g *GitHub) Called(call string) bool {
	return slices.Contains(g.Calls(), call)
}

// Creates returns the decoded bodies of every create request.
func (g *GitHub) Creates() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.creates)
}

// Codespace returns the named codespace, if it exists.
func (g *GitHub) Codespace(name string) (github.Codespace, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var found github.Codespace

	entry, ok := g.spaces[name]
	if ok {
		found = entry.codespace
	}

	return found, ok
}

// RoundTrip serves one request.
func (g *GitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	err := req.Context().Err()
	if err != nil {
		return nil, fmt.Errorf("serve %s: %w", req.URL.Path, err)
	}

	var body []byte

	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()

		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	call := req.Method + " " + req.URL.Path

	g.calls = append(g.calls, call)

	code, failing := g.Fail[call]
	if failing {
		return respond(req, code, map[string]string{messageKey: "injected failure"})
	}

	return g.route(req, body)
}

// route serves req by its method and path.
func (g *GitHub) route(req *http.Request, body []byte) (*http.Response, error) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")

	switch {
	case matches(req, http.MethodPost, parts, "repos", "*", "*", codespacesSegment):
		return g.serveCreate(req, parts[1], parts[2], body)
	case matches(req, http.MethodGet, parts, "user", codespacesSegment):
		return g.serveList(req)
	case matches(req, http.MethodGet, parts, "user", codespacesSegment, "*"):
		return g.serveGet(req, parts[2])
	case matches(req, http.MethodPost, parts, "user", codespacesSegment, "*", "stop"):
		return g.serveStop(req, parts[2])
	case matches(req, http.MethodDelete, parts, "user", codespacesSegment, "*"):
		return g.serveDelete(req, parts[2])
	}

	return respond(req, http.StatusNotFound, map[string]string{messageKey: "no such endpoint"})
}

// matches reports whether req has method and the path segments pattern,
// where "*" matches any one segment.
func matches(req *http.Request, method string, parts []string, pattern ...string) bool {
	if req.Method != method || len(parts) != len(pattern) {
		return false
	}

	for i, want := range pattern {
		if want != "*" && want != parts[i] {
			return false
		}
	}

	return true
}

func (g *GitHub) add(codespace github.Codespace, states []string) {
	if g.spaces == nil {
		g.spaces = make(map[string]*space)
	}

	g.spaces[codespace.Name] = &space{codespace: codespace, states: states}
	g.order = append(g.order, codespace.Name)
}

// serveCreate serves POST /repos/{owner}/{repo}/codespaces.
func (g *GitHub) serveCreate(req *http.Request, owner, repo string, body []byte) (*http.Response, error) {
	var fields map[string]any

	err := json.Unmarshal(body, &fields)
	if err != nil {
		return respond(req, http.StatusBadRequest, nil)
	}

	g.creates = append(g.creates, fields)
	if g.OnCreate != nil {
		g.OnCreate()
	}

	codespace := github.Codespace{
		Name:                   g.newName(),
		DisplayName:            fmt.Sprint(fields["display_name"]),
		State:                  "",
		Repository:             github.Repository{Name: repo, FullName: owner + "/" + repo},
		Machine:                &github.Machine{Name: fmt.Sprint(fields["machine"])},
		CreatedAt:              time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC),
		IdleTimeoutMinutes:     nil,
		RetentionPeriodMinutes: nil,
		RetentionExpiresAt:     nil,
	}
	g.add(codespace, slices.Clone(g.States))

	return respond(req, http.StatusCreated, g.view(g.spaces[codespace.Name]))
}

// serveList serves GET /user/codespaces.
func (g *GitHub) serveList(req *http.Request) (*http.Response, error) {
	list := []github.Codespace{}

	for _, name := range g.order {
		entry, ok := g.spaces[name]
		if ok {
			list = append(list, entry.codespace)
		}
	}

	return respond(req, http.StatusOK, map[string]any{"total_count": len(list), codespacesSegment: list})
}

// serveGet serves GET /user/codespaces/{name}.
func (g *GitHub) serveGet(req *http.Request, name string) (*http.Response, error) {
	entry, ok := g.spaces[name]
	if !ok {
		return notFound(req)
	}

	return respond(req, http.StatusOK, g.view(entry))
}

// serveStop serves POST /user/codespaces/{name}/stop.
func (g *GitHub) serveStop(req *http.Request, name string) (*http.Response, error) {
	entry, ok := g.spaces[name]
	if !ok {
		return notFound(req)
	}

	entry.states, entry.codespace.State = nil, "Shutdown"

	return respond(req, http.StatusOK, entry.codespace)
}

// serveDelete serves DELETE /user/codespaces/{name}.
func (g *GitHub) serveDelete(req *http.Request, name string) (*http.Response, error) {
	_, ok := g.spaces[name]
	if !ok {
		return notFound(req)
	}

	delete(g.spaces, name)

	return respond(req, http.StatusAccepted, nil)
}

func notFound(req *http.Request) (*http.Response, error) {
	return respond(req, http.StatusNotFound, map[string]string{messageKey: "Not Found"})
}

// newName returns the next cs-N name not already taken.
func (g *GitHub) newName() string {
	for {
		g.next++

		name := fmt.Sprintf("cs-%d", g.next)
		if _, taken := g.spaces[name]; !taken {
			return name
		}
	}
}

// view advances entry to its next state and returns it.
func (g *GitHub) view(entry *space) github.Codespace {
	switch len(entry.states) {
	case 0:
		if entry.codespace.State == "" {
			entry.codespace.State = "Available"
		}
	case 1:
		entry.codespace.State = entry.states[0]
	default:
		entry.codespace.State, entry.states = entry.states[0], entry.states[1:]
	}

	return entry.codespace
}

func respond(req *http.Request, code int, body any) (*http.Response, error) {
	var encoded []byte

	if body != nil {
		var err error

		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode response: %w", err)
		}
	}

	resp := new(http.Response)
	resp.StatusCode = code
	resp.Status = fmt.Sprintf("%d %s", code, http.StatusText(code))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.Request = req

	return resp, nil
}

// Handler runs one command on a fake Conn. name is the sandbox name.
type Handler func(ctx context.Context, name string, cmd remote.Cmd) (int, error)

// Default answers runspace's prepare and diff steps like a healthy sandbox
// at HeadSHA and exits 0 for anything else.
func Default(_ context.Context, _ string, cmd remote.Cmd) (int, error) {
	switch Step(cmd) {
	case stepPrepare:
		want := cmd.Argv[4]
		if want != "" && want != HeadSHA {
			_, err := fmt.Fprintf(cmd.Stderr, "HEAD is %s, not %s\n", HeadSHA, want)
			if err != nil {
				return 0, fmt.Errorf("write the prepare error: %w", err)
			}

			return 1, nil
		}

		_, err := fmt.Fprintln(cmd.Stdout, HeadSHA)
		if err != nil {
			return 0, fmt.Errorf("write the base: %w", err)
		}
	case stepDiff:
		_, err := io.WriteString(cmd.Stdout, Patch)
		if err != nil {
			return 0, fmt.Errorf("write the patch: %w", err)
		}
	}

	return 0, nil
}

// Step names the runspace step cmd runs: "prepare", "diff", or "" for a
// caller's own command.
func Step(cmd remote.Cmd) string {
	if len(cmd.Argv) < 4 || cmd.Argv[0] != "sh" || cmd.Argv[1] != "-c" || cmd.Argv[3] != "runspace" {
		return ""
	}

	switch script := cmd.Argv[2]; {
	case strings.Contains(script, "update-ref"):
		return stepPrepare
	case strings.Contains(script, "diff --cached"):
		return stepDiff
	}

	return ""
}

// ErrClosed is returned by Exec on a closed Conn.
var ErrClosed = errors.New("sandboxtest: connection closed")

// Conn fakes one SSH connection.
type Conn struct {
	Name    string
	handler Handler

	mu     sync.Mutex
	cmds   []remote.Cmd
	closed bool
	done   chan struct{}
}

// Exec records cmd and runs the handler. Closing the Conn interrupts the
// handler, as a real connection does: its context ends with ErrClosed.
func (c *Conn) Exec(ctx context.Context, cmd remote.Cmd) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return 0, ErrClosed
	}

	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		select {
		case <-c.done:
			cancel(ErrClosed)
		case <-ctx.Done():
		}
	}()

	code, err := c.handler(ctx, c.Name, cmd)
	if err != nil && errors.Is(context.Cause(ctx), ErrClosed) {
		return 0, ErrClosed
	}

	return code, err
}

// Close marks the Conn closed.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.closed {
		c.closed = true
		close(c.done)
	}

	return nil
}

// Done returns a channel closed by Close, as remote.Conn's is.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Cmds returns every command run so far.
func (c *Conn) Cmds() []remote.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.cmds)
}

// Dialer hands out fake Conns.
type Dialer struct {
	// Handler runs every command. Nil means Default.
	Handler Handler
	// Err, when set, fails every dial.
	Err error
	// Hook, when set, runs inside each dial before it returns, for tests
	// that need to hold a dial open.
	Hook func(ctx context.Context, name string) error

	mu    sync.Mutex
	conns []*Conn
}

// Dial returns a new Conn.
func (d *Dialer) Dial(ctx context.Context, name string, _ func(string)) (*Conn, error) {
	if d.Hook != nil {
		err := d.Hook(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("dial hook: %w", err)
		}
	}

	if d.Err != nil {
		return nil, d.Err
	}

	handler := d.Handler
	if handler == nil {
		handler = Default
	}

	conn := &Conn{
		Name:    name,
		handler: handler,
		mu:      sync.Mutex{},
		cmds:    nil,
		closed:  false,
		done:    make(chan struct{}),
	}

	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()

	return conn, nil
}

// Conns returns every Conn dialed so far, in order.
func (d *Dialer) Conns() []*Conn {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.conns)
}

// Dials counts the dials of name.
func (d *Dialer) Dials(name string) int {
	count := 0

	for _, conn := range d.Conns() {
		if conn.Name == name {
			count++
		}
	}

	return count
}
