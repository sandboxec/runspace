// Command runspace runs commands in disposable GitHub codespaces. It has two
// front ends over the same seven commands: a CLI, and a stdio MCP server
// (runspace mcp) for AI agents.
//
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"charm.land/log/v2"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// errTTYNeedsTerminal is returned when exec -t runs without a terminal on
// stdin.
var errTTYNeedsTerminal = errors.New("-t needs stdin to be a terminal")

func main() {
	// The first SIGINT or SIGTERM cancels ctx, which signals the remote
	// command and applies --keep. stop then restores the default handling, so
	// a second signal kills the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()

	cli := newApp()
	code := execute(ctx, newRoot(cli), cli.log, os.Args[1:])

	stop()
	os.Exit(code)
}

// execute runs the command line on root and returns the process exit code.
// It logs each error once.
func execute(ctx context.Context, root *cobra.Command, logger *log.Logger, args []string) int {
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)

	msg := errorLine(err)
	if msg != "" {
		logger.Error(msg)
	}

	return exitCode(cmd, err)
}

// exitCode returns the process exit code. exec and run pass the remote code
// through, with 124, 125, and 130 for runspace's own outcomes; any other
// error in them, including a usage error, is 125. Every other command exits
// 0 or 1.
func exitCode(cmd *cobra.Command, err error) int {
	switch {
	case err == nil:
		return 0
	case isCommandRunner(cmd):
		return sandbox.ExitCode(err)
	default:
		return 1
	}
}

func isCommandRunner(cmd *cobra.Command) bool {
	return cmd != nil && (cmd.Name() == "exec" || cmd.Name() == "run")
}

// errorLine returns the message to log for err, or "" for success and for
// a plain non-zero remote exit, which logs nothing.
func errorLine(err error) string {
	var ee *sandbox.ExitError
	if err == nil || errors.As(err, &ee) && ee.Err == nil {
		return ""
	}

	return err.Error()
}

// app holds what the commands need from the process, so tests can replace
// it.
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// tty is stdin when stdin is a terminal, else nil.
	tty *os.File
	// log receives runspace's own messages. It starts on stderr at
	// InfoLevel; configureLog applies the settings once flags are parsed.
	log *log.Logger
	// getenv reads the log settings.
	getenv func(string) string
	// verbose is --verbose.
	verbose bool
	// dir is the working directory, where the CLI looks for a git
	// repository when create and run get no -R. It is "" when the working
	// directory is unknown.
	dir string
	// token is -T/--token, or "" to use RUNSPACE_TOKEN.
	token string
	// newSandboxes returns the Sandboxes a command runs with, using token
	// over RUNSPACE_TOKEN. reuse keeps connections across calls (MCP).
	newSandboxes func(token string, reuse bool) (*sandbox.Sandboxes, error)
}

func newApp() *app {
	var tty *os.File
	if sandbox.IsTerminal(os.Stdin) {
		tty = os.Stdin
	}

	// A working directory that cannot be read only means no git repository
	// is found.
	dir, err := os.Getwd()
	if err != nil {
		dir = ""
	}

	logger := newLogger(os.Stderr)

	return &app{
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		tty:     tty,
		log:     logger,
		getenv:  os.Getenv,
		verbose: false,
		dir:     dir,
		token:   "",
		newSandboxes: func(token string, reuse bool) (*sandbox.Sandboxes, error) {
			// Nil Getenv, APIClient, and Dial select the production ones.
			sandboxes, err := sandbox.New(sandbox.Config{
				Token: token, Getenv: nil, APIClient: nil, Dial: nil, Logger: logger, Reuse: reuse,
			})

			return sandboxes, relay(err)
		},
	}
}

// progress returns a function that logs progress lines at InfoLevel.
func (a *app) progress() func(string) {
	return func(line string) { a.log.Info(line) }
}

// repo returns the repository for create and run: given when -R was passed,
// else the one the working directory's git remote origin names. The second
// case is implicit and decides what is billed, so it is announced.
func (a *app) repo(given string) (string, error) {
	if given != "" {
		return given, nil
	}

	repo, err := originRepo(a.dir)
	if err != nil {
		return "", err
	}

	a.log.Info("Using " + repo + " from git remote origin")

	return repo, nil
}

// streams returns the streams for a command: its output and progress, and
// stdin when the command reads it (nil for none).
func (a *app) streams(stdin io.Reader) sandbox.Streams {
	return sandbox.Streams{Stdin: stdin, Stdout: a.stdout, Stderr: a.stderr, TTY: nil, Progress: a.progress()}
}

// stdinIf returns stdin when attach is set, as -i asks, else nil.
func (a *app) stdinIf(attach bool) io.Reader {
	if attach {
		return a.stdin
	}

	return nil
}

// withSandboxes opens Sandboxes for one CLI command, runs work with ctx, and
// closes them after. Every caller validates its options first, so an input
// error is reported before a token or network error.
func (a *app) withSandboxes(ctx context.Context, work func(context.Context, *sandbox.Sandboxes) error) error {
	sandboxes, err := a.newSandboxes(a.token, false)
	if err != nil {
		return err
	}

	// The command's own result matters more than a failure to close.
	defer func() { _ = sandboxes.Close() }()

	return work(ctx, sandboxes)
}

// relay returns err with the same text and the same chain, so errors.Is and
// errors.As still match. It marks where an error from another package
// crosses into main, which prints it as is.
func relay(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%w", err)
}
