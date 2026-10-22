//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.sandbox.ec/runspace/internal/sandbox"
)

var (
	errPatchTooLarge = fmt.Errorf(
		"the patch is larger than %d KiB; set output to write it to a host file", outputCapKiB)
	errDashStream = errors.New(`"-" streams are not available over MCP; copy to or from a host path`)
)

// handlers are the seven tools, over one set of sandboxes.
type handlers struct {
	sandboxes *sandbox.Sandboxes
}

func (h *handlers) create(
	ctx context.Context, report func(string), input sandbox.CreateOptions,
) (*mcp.CallToolResult, sandbox.CreateResult, error) {
	res, err := h.sandboxes.Create(ctx, input, report)

	return nil, res, passThrough(err)
}

// execInput is exec's options plus stdin, whose presence attaches it.
type execInput struct {
	sandbox.ExecOptions

	Stdin *string `json:"stdin,omitempty" jsonschema:"text sent to the command's stdin; without it the command reads EOF"`
}

// commandOutput is the result of a remote command.
//
//nolint:tagliatelle // The JSON names are the documented result fields of exec and run.
type commandOutput struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

func (h *handlers) exec(
	ctx context.Context, report func(string), input execInput,
) (*mcp.CallToolResult, commandOutput, error) {
	stdout, stderr := newTail(), newTail()
	err := h.sandboxes.Exec(ctx, input.ExecOptions, streams(stdinOf(input.Stdin), stdout, stderr, report))
	out, err := commandResult(err, stdout, stderr)

	return nil, out, err
}

// runInput is run's options plus stdin.
type runInput struct {
	sandbox.RunOptions

	Stdin *string `json:"stdin,omitempty" jsonschema:"text sent to the command's stdin; without it the command reads EOF"`
}

type runOutput struct {
	commandOutput

	Name string `json:"name"`
	Kept bool   `json:"kept"`
}

func (h *handlers) run(
	ctx context.Context, report func(string), input runInput,
) (*mcp.CallToolResult, runOutput, error) {
	stdout, stderr := newTail(), newTail()
	res, err := h.sandboxes.Run(ctx, input.RunOptions, streams(stdinOf(input.Stdin), stdout, stderr, report))
	notice := runNotice(res)

	out, err := commandResult(err, stdout, stderr)
	if err != nil {
		// A tool error drops the result, so the notice goes in the error.
		if notice != "" {
			err = fmt.Errorf("%w\n%s", err, notice)
		}

		return nil, runOutput{}, err
	}

	if notice != "" {
		out.Stderr += "\n" + notice
	}

	return nil, runOutput{commandOutput: out, Name: res.Name, Kept: res.Kept}, nil
}

// runNotice says what happened to run's sandbox when the agent must act:
// a kept sandbox to delete later, or a failed cleanup.
func runNotice(res sandbox.RunResult) string {
	switch {
	case res.Name == "":
		return ""
	case res.CleanupErr != nil && res.Kept:
		return fmt.Sprintf("runspace: sandbox %s is kept but could not be stopped (%v); "+
			"its idle timeout stops it. Delete it when done.", res.Name, res.CleanupErr)
	case res.CleanupErr != nil:
		return fmt.Sprintf("runspace: sandbox %s could not be deleted (%v); "+
			"delete it with the delete tool.", res.Name, res.CleanupErr)
	case res.Kept:
		return fmt.Sprintf("runspace: sandbox %s is kept, stopped; it is deleted %s after it stops. "+
			"Inspect it with exec, then delete it with the delete tool.", res.Name, sandbox.FormatDuration(res.Retention))
	}

	return ""
}

// commandResult maps the exit-code rule to MCP: a runspace failure is a
// tool error carrying the message, and every other code, including 124 and
// a command's own 125, is a normal result. runspace's own notes, such as
// a timeout, are added to stderr.
func commandResult(err error, stdout, stderr *tail) (commandOutput, error) {
	if runspaceFailed(err) {
		return commandOutput{}, err
	}

	var exitErr *sandbox.ExitError
	if errors.As(err, &exitErr) && exitErr.Err != nil {
		// A tail never fails to write.
		_, _ = fmt.Fprintf(stderr, "\nrunspace: %v\n", exitErr.Err)
	}

	return commandOutput{
		ExitCode:  sandbox.ExitCode(err),
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}, nil
}

// runspaceFailed reports whether err is runspace's own failure (125 with a
// cause), as opposed to an outcome of the remote command.
func runspaceFailed(err error) bool {
	var exitErr *sandbox.ExitError
	if !errors.As(err, &exitErr) {
		return err != nil
	}

	return exitErr.Code == sandbox.ExitFailure && exitErr.Err != nil
}

// streams returns the streams of a call, which never has a terminal.
func streams(stdin io.Reader, stdout, stderr io.Writer, report func(string)) sandbox.Streams {
	return sandbox.Streams{Stdin: stdin, Stdout: stdout, Stderr: stderr, TTY: nil, Progress: report}
}

func stdinOf(text *string) io.Reader {
	if text == nil {
		return nil
	}

	return strings.NewReader(*text)
}

// diff returns the patch as text, or the path it wrote. A patch over the
// output cap fails, because a truncated patch is useless.
func (h *handlers) diff(
	ctx context.Context, report func(string), input sandbox.DiffOptions,
) (*mcp.CallToolResult, any, error) {
	var patch boundedBuffer

	path, err := h.sandboxes.Diff(ctx, input, streams(nil, &patch, nil, report))

	switch {
	case err != nil:
		return nil, nil, passThrough(err)
	case path != "":
		return textResult("wrote the patch to " + path), nil, nil
	case patch.overflow:
		return nil, nil, errPatchTooLarge
	}

	return textResult(patch.String()), nil, nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Meta:              nil,
		Content:           []mcp.Content{&mcp.TextContent{Text: text, Meta: nil, Annotations: nil}},
		StructuredContent: nil,
		IsError:           false,
		InputRequests:     nil,
		RequestState:      "",
	}
}

func (h *handlers) cp(
	ctx context.Context, report func(string), input sandbox.CopyOptions,
) (*mcp.CallToolResult, sandbox.CopyResult, error) {
	// Validate first, so an invalid pair fails with the CLI's message.
	err := input.Validate()
	if err != nil {
		return nil, sandbox.CopyResult{}, passThrough(err)
	}

	// "-" streams have no MCP equivalent (the transport list).
	if input.Src == "-" || input.Dst == "-" {
		return nil, sandbox.CopyResult{}, errDashStream
	}

	res, err := h.sandboxes.Copy(ctx, input, streams(nil, nil, nil, report))

	return nil, res, passThrough(err)
}

type lsOutput struct {
	Sandboxes []sandbox.Sandbox `json:"sandboxes"`
}

func (h *handlers) ls(
	ctx context.Context, _ func(string), input sandbox.ListOptions,
) (*mcp.CallToolResult, lsOutput, error) {
	list, err := h.sandboxes.List(ctx, input)

	return nil, lsOutput{Sandboxes: list}, passThrough(err)
}

type deleteOutput struct {
	Deleted []string `json:"deleted"`
}

func (h *handlers) delete(
	ctx context.Context, _ func(string), input sandbox.DeleteOptions,
) (*mcp.CallToolResult, deleteOutput, error) {
	deleted, err := h.sandboxes.Delete(ctx, input)
	if err != nil && len(deleted) > 0 {
		// A tool error drops the result, so name what was deleted.
		err = fmt.Errorf("deleted %s; %w", strings.Join(deleted, ", "), err)
	}

	return nil, deleteOutput{Deleted: deleted}, passThrough(err)
}
