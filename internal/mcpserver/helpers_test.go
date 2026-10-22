//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"charm.land/log/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.sandbox.ec/runspace/internal/mcpserver"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// Tool names and argument keys that several tests share.
const (
	toolCreate = "create"
	toolExec   = "exec"
	toolRun    = "run"
	toolDiff   = "diff"
	toolCp     = "cp"
	toolLs     = "ls"
	toolDelete = "delete"

	keyArgv = "argv"
	keyName = "name"
	keyRepo = "repo"
	keyKeep = "keep"
	keySrc  = "src"
	keyDst  = "dst"

	testRepo          = "o/r"
	testName          = "cs-1"
	stateProvisioning = "Provisioning"

	// stepDiff is the runspace step that prints the patch.
	stepDiff = "diff"

	// cmdLong names a scripted command that blocks until it is cancelled.
	cmdLong = "long"
)

var errConnectionLost = errors.New("connection lost")

// fixture is an MCP client connected to a server over in-memory fakes.
type fixture struct {
	github    *sandboxtest.GitHub
	dialer    *sandboxtest.Dialer
	sandboxes *sandbox.Sandboxes
	client    *mcp.ClientSession
	progress  chan string
	// logs holds the server's log, at DebugLevel.
	logs *lockedBuffer
}

// lockedBuffer is a buffer that the server's goroutines write while a test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A bytes.Buffer write never fails.
	_, _ = b.buf.Write(data)

	return len(data), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// newDebugLogger returns a logger at DebugLevel, so tests run every log
// call, and the buffer it writes.
func newDebugLogger() (*log.Logger, *lockedBuffer) {
	return newBufferLogger(log.DebugLevel)
}

// newBufferLogger returns a logger at level and the buffer it writes.
func newBufferLogger(level log.Level) (*log.Logger, *lockedBuffer) {
	logs := new(lockedBuffer)
	logger := log.New(logs)
	logger.SetLevel(level)

	return logger, logs
}

func newSandboxes(
	t *testing.T, github *sandboxtest.GitHub, dialer *sandboxtest.Dialer, logger *log.Logger,
) *sandbox.Sandboxes {
	t.Helper()

	sandboxes, err := sandbox.New(sandbox.Config{
		Token:     "",
		Getenv:    func(string) string { return "" },
		APIClient: github.Client(),
		Dial: func(
			ctx context.Context, name string, admit func(remote.Codespace) error, report func(string),
		) (sandbox.Conn, error) {
			conn, err := dialer.Dial(ctx, name, admit, report)
			if err != nil {
				return nil, fmt.Errorf("dial %s: %w", name, err)
			}

			return conn, nil
		},
		Logger: logger,
		Reuse:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	return sandboxes
}

func newFixture(t *testing.T, handler sandboxtest.Handler) *fixture {
	t.Helper()

	logger, logs := newDebugLogger()
	github := new(sandboxtest.GitHub)
	dialer := newDialer(github, handler)
	sandboxes := newSandboxes(t, github, dialer, logger)
	progress := make(chan string, 64)
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := mcpserver.New(sandboxes, logger, "test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	options := new(mcp.ClientOptions)
	options.ProgressNotificationHandler = func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
		progress <- req.Params.Message
	}

	client, err := mcp.NewClient(clientInfo(), options).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = serverSession.Wait()
		_ = sandboxes.Close()
	})

	return &fixture{github: github, dialer: dialer, sandboxes: sandboxes, client: client, progress: progress, logs: logs}
}

// newDialer returns a Dialer of the codespaces in github whose Conns run
// handler; nil runs the defaults.
func newDialer(github *sandboxtest.GitHub, handler sandboxtest.Handler) *sandboxtest.Dialer {
	return &sandboxtest.Dialer{GitHub: github, Handler: handler, Err: nil, Hook: nil}
}

func clientInfo() *mcp.Implementation {
	return &mcp.Implementation{
		Name:        "test",
		Version:     "",
		Title:       "",
		Description: "",
		WebsiteURL:  "",
		Icons:       nil,
	}
}

// toolCall returns the parameters of a call to tool.
func toolCall(tool string, args any) *mcp.CallToolParams {
	return &mcp.CallToolParams{
		Meta:           nil,
		Name:           tool,
		Arguments:      args,
		InputResponses: nil,
		RequestState:   "",
	}
}

// call calls a tool and returns its result.
func (f *fixture) call(t *testing.T, tool string, args any) *mcp.CallToolResult {
	t.Helper()

	return callWith(t, f.client, toolCall(tool, args))
}

// callWith makes the call params describe and returns its result.
func callWith(t *testing.T, client *mcp.ClientSession, params *mcp.CallToolParams) *mcp.CallToolResult {
	t.Helper()

	res, err := client.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: %v", params.Name, err)
	}

	return res
}

// callInBackground calls a tool whose result the test does not need.
func callInBackground(client *mcp.ClientSession, tool string, args any) {
	go func() { _, _ = client.CallTool(context.Background(), toolCall(tool, args)) }()
}

// text returns a result's text content.
func text(res *mcp.CallToolResult) string {
	var builder strings.Builder

	for _, content := range res.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			builder.WriteString(textContent.Text)
		}
	}

	return builder.String()
}

// decode unmarshals a result's structured content into target.
func decode(t *testing.T, res *mcp.CallToolResult, target any) {
	t.Helper()

	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(encoded, target)
	if err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
}

func decodeCommand(t *testing.T, res *mcp.CallToolResult) mcpserver.CommandOutput {
	t.Helper()

	var out mcpserver.CommandOutput

	decode(t, res, &out)

	return out
}

func decodeRun(t *testing.T, res *mcp.CallToolResult) mcpserver.RunOutput {
	t.Helper()

	var out mcpserver.RunOutput

	decode(t, res, &out)

	return out
}

// sandboxName returns the name a create result carries.
func sandboxName(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	var created sandbox.CreateResult

	decode(t, res, &created)

	return created.Name
}

func (f *fixture) create(t *testing.T) string {
	t.Helper()

	res := f.call(t, toolCreate, map[string]any{keyRepo: testRepo})
	if res.IsError {
		t.Fatalf("create: %s", text(res))
	}

	return sandboxName(t, res)
}

// execArgs returns the arguments of an exec of argv in the sandbox name.
func execArgs(name string, argv ...string) map[string]any {
	// An empty argv is an empty list, not null.
	return map[string]any{keyName: name, keyArgv: append([]string{}, argv...)}
}

// with sets key in args and returns args.
func with(args map[string]any, key string, value any) map[string]any {
	args[key] = value

	return args
}

// userCmd answers runspace's own steps and runs script for the caller's
// command.
func userCmd(script func(ctx context.Context, cmd remote.Cmd) (int, error)) sandboxtest.Handler {
	return func(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
		if sandboxtest.Step(cmd) != "" {
			return sandboxtest.Default(ctx, name, cmd)
		}

		return script(ctx, cmd)
	}
}

// reply writes output to a scripted command's stream.
func reply(t *testing.T, stream io.Writer, output string) {
	t.Helper()

	_, err := io.WriteString(stream, output)
	if err != nil {
		t.Error(err)
	}
}

// blockUntilCancelled is a scripted command that runs until its context
// ends.
func blockUntilCancelled(ctx context.Context, _ remote.Cmd) (int, error) {
	<-ctx.Done()

	return 0, fmt.Errorf("command stopped: %w", context.Cause(ctx))
}
