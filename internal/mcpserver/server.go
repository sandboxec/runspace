// Package mcpserver is runspace's stdio MCP server. It exposes one tool per
// command, built from the same options structs as the CLI. The main
// differences from the CLI come from the transport: stdin is a string field,
// streams have no equivalent, output is capped, repo has no git-origin
// default, and run and delete word their notices for a tool result.
//
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"charm.land/log/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// maxCalls bounds the calls that run at once across the server, whichever
// tools they are. sshd's default MaxSessions of 10 limits the sessions on one
// SSH connection, and exec, diff, and cp share one cached connection for each
// sandbox; with at most maxCalls calls in flight, those sessions stay under it.
const maxCalls = 8

// Serve runs the MCP server over stdin and stdout until stdin reaches EOF or
// ctx is done. It takes ownership of sandboxes: on return every connection is
// closed, and every sandbox is left as it is. EOF cancels the calls in
// flight, so a pending create deletes its half-made sandbox.
//
// When ctx ends, as on SIGINT, the calls in flight are cancelled too, and
// Serve returns nil once they finish.
//
// logger receives the SDK's own log and a debug line for each tool call;
// nil discards them. It must not write to stdout, which carries the protocol.
func Serve(
	ctx context.Context,
	sandboxes *sandbox.Sandboxes,
	logger *log.Logger,
	version string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	// A failed close leaves nothing to recover: Serve returns as the process
	// exits.
	defer func() { _ = sandboxes.Close() }()

	transport := &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}, MaxLineLength: 0}

	err := newServer(ctx, sandboxes, logger, version).Run(ctx, transport)
	if err != nil && ctx.Err() == nil {
		return passThrough(err)
	}

	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// New returns the server with its seven tools, for callers that connect it
// to their own transport, such as the parity tests. logger is as for Serve.
func New(sandboxes *sandbox.Sandboxes, logger *log.Logger, version string) *mcp.Server {
	return newServer(context.Background(), sandboxes, logger, version)
}

// newServer returns the server; ending base cancels every call in flight.
// The SDK waits for handlers when its context ends but does not cancel
// them.
func newServer(base context.Context, sandboxes *sandbox.Sandboxes, logger *log.Logger, version string) *mcp.Server {
	if logger == nil {
		logger = log.New(io.Discard)
	}

	options := new(mcp.ServerOptions)
	options.Instructions = instructions
	options.Logger = slog.New(debugOnly{logger})

	implementation := &mcp.Implementation{
		Name:        "runspace",
		Version:     version,
		Title:       "",
		Description: "",
		WebsiteURL:  "",
		Icons:       nil,
	}
	server := mcp.NewServer(implementation, options)
	tools := &handlers{sandboxes: sandboxes}
	slots := make(chan struct{}, maxCalls)

	mcp.AddTool(server, newTool("create", createDescription), limit(base, slots, logger, tools.create))
	mcp.AddTool(server, newTool("exec", execDescription), limit(base, slots, logger, tools.exec))
	mcp.AddTool(server, newTool("run", runDescription), limit(base, slots, logger, tools.run))
	mcp.AddTool(server, newTool("diff", diffDescription), limit(base, slots, logger, tools.diff))
	mcp.AddTool(server, newTool("cp", cpDescription), limit(base, slots, logger, tools.cp))
	mcp.AddTool(server, newTool("ls", lsDescription), limit(base, slots, logger, tools.ls))
	mcp.AddTool(server, newTool("delete", deleteDescription), limit(base, slots, logger, tools.delete))

	return server
}

// newTool returns the tool the SDK completes with schemas built from its
// handler's types.
func newTool(name, description string) *mcp.Tool {
	tool := new(mcp.Tool)
	tool.Name = name
	tool.Description = unwrap(description)

	return tool
}

// handler is one tool. report sends a progress line, and is nil when the
// client asked for none.
type handler[In, Out any] func(ctx context.Context, report func(string), input In) (*mcp.CallToolResult, Out, error)

// limit runs call in one of slots, on a context that also ends with base. The
// SDK dispatches calls concurrently and has no limit of its own. Every call,
// queued or running, sends a heartbeat when the client asked for progress.
// A call that runs logs how it ended.
func limit[In, Out any](
	base context.Context, slots chan struct{}, logger *log.Logger, call handler[In, Out],
) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)

		stop := context.AfterFunc(base, func() { cancel(context.Cause(base)) })
		defer stop()

		report := progress(ctx, req)
		defer heartbeat(report)()

		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			var zero Out

			return nil, zero, context.Cause(ctx)
		}

		start := time.Now()
		res, out, err := call(ctx, report, input)
		// Not the error's text, which the client gets and which can quote
		// the call's input, such as its command line.
		logger.Debug("The tool call ended", "tool", req.Params.Name, "failed", err != nil,
			"took", time.Since(start).Round(time.Millisecond))

		return res, out, err
	}
}

// debugOnly passes the SDK's own log on at DebugLevel. Its session events,
// and the error it logs when a signal ends a server normally, are detail;
// a failure that matters ends Serve with an error.
type debugOnly struct{ slog.Handler }

func (h debugOnly) Enabled(ctx context.Context, _ slog.Level) bool {
	return h.Handler.Enabled(ctx, slog.LevelDebug)
}

func (h debugOnly) Handle(ctx context.Context, record slog.Record) error {
	record.Level = slog.LevelDebug

	err := h.Handler.Handle(ctx, record)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func (h debugOnly) WithAttrs(attrs []slog.Attr) slog.Handler {
	return debugOnly{h.Handler.WithAttrs(attrs)}
}

func (h debugOnly) WithGroup(name string) slog.Handler {
	return debugOnly{h.Handler.WithGroup(name)}
}

// passThrough returns err with its message unchanged: the CLI prints the
// same message, so the tool error must read the same.
func passThrough(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%w", err)
}
