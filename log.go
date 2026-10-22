//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/mattn/go-colorable"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// The log settings. Logs carry runspace's own messages: errors, warnings,
// progress, and debug detail. Command output never goes to the log.
const (
	envLogLevel  = "RUNSPACE_LOG_LEVEL"
	envLogOutput = "RUNSPACE_LOG_OUTPUT"
	verboseFlag  = "verbose"

	// logFileMode keeps a new log file to its owner, as debug lines name
	// sandboxes and repositories.
	logFileMode os.FileMode = 0o600
)

var (
	errLogLevel    = errors.New("want debug, info, warn, error, or fatal")
	errLogOnStdout = errors.New("the file is stdout, which carries the MCP protocol; " +
		"name another file, or leave " + envLogOutput + " empty to log to stderr")
)

// newLogger returns a logger that writes to w at InfoLevel, starting each
// line with the time of day.
func newLogger(w io.Writer) *log.Logger {
	logger := log.New(io.Discard)
	logger.SetReportTimestamp(true)
	logger.SetTimeFormat(time.Kitchen)
	setLogOutput(logger, w)

	return logger
}

// setLogOutput points logger at w. Only a terminal gets colors, through
// go-colorable, which renders them on Windows consoles that lack virtual
// terminal processing. Anything else gets plain text.
func setLogOutput(logger *log.Logger, w io.Writer) {
	file, isFile := w.(*os.File)
	if !isFile || !sandbox.IsTerminal(file) {
		logger.SetOutput(w)
		logger.SetColorProfile(colorprofile.NoTTY)

		return
	}

	logger.SetOutput(colorable.NewColorable(file))

	// The profile comes from the environment alone: colorprofile.Detect
	// would also run tmux inside tmux. colorprofile turns colors off only
	// for a NO_COLOR that parses as true, and https://no-color.org asks for
	// any value but empty.
	profile := colorprofile.Env(os.Environ())
	if os.Getenv("NO_COLOR") != "" {
		profile = min(profile, colorprofile.ASCII)
	}

	logger.SetColorProfile(profile)
}

// configureLog applies --verbose, or else RUNSPACE_LOG_LEVEL, and
// RUNSPACE_LOG_OUTPUT to a.log.
func (a *app) configureLog(cmd *cobra.Command) error {
	level := log.DebugLevel

	if !a.verbose {
		var err error

		level, err = a.logLevel()
		if err != nil {
			return err
		}
	}

	out, err := a.logOutput(cmd)
	if err != nil {
		return err
	}

	setLogOutput(a.log, out)
	a.log.SetLevel(level)

	return nil
}

// logLevel returns the level that RUNSPACE_LOG_LEVEL names, InfoLevel by
// default.
func (a *app) logLevel() (log.Level, error) {
	name := a.getenv(envLogLevel)
	if name == "" {
		return log.InfoLevel, nil
	}

	level, err := log.ParseLevel(name)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", envLogLevel, name, errLogLevel)
	}

	return level, nil
}

// logOutput returns where RUNSPACE_LOG_OUTPUT sends the log: stderr when it
// is empty, or else the file it names, which the log is appended to. mcp
// refuses a file that is its own stdout, such as /dev/stdout.
func (a *app) logOutput(cmd *cobra.Command) (io.Writer, error) {
	name := a.getenv(envLogOutput)
	if name == "" {
		return a.stderr, nil
	}

	// The process holds the file until it exits. Its path comes from whoever
	// starts runspace.
	//nolint:gosec // G304: the operator chooses the log file with RUNSPACE_LOG_OUTPUT.
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envLogOutput, err)
	}

	if cmd.Name() == "mcp" && sameFile(file, a.stdout) {
		_ = file.Close()

		return nil, fmt.Errorf("%s=%s: %w", envLogOutput, name, errLogOnStdout)
	}

	return file, nil
}

// sameFile reports whether w is a file and the same one as file.
func sameFile(file *os.File, w io.Writer) bool {
	other, isFile := w.(*os.File)
	if !isFile {
		return false
	}

	info, err := file.Stat()
	if err != nil {
		return false
	}

	otherInfo, err := other.Stat()

	return err == nil && os.SameFile(info, otherInfo)
}
