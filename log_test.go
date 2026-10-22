package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The levels that start log lines, as the text formatter pads them.
const (
	debugLevel = "DEBUG "
	infoLevel  = "INFO  "
	warnLevel  = "WARN  "
	errorLevel = "ERROR "

	createProgress = infoLevel + "Creating a sandbox from o/r"
)

// createArgs creates a sandbox from repoName.
func createArgs() []string { return []string{cmdCreate, repoFlag, repoName} }

// By default the log is on stderr at InfoLevel, without colors when stderr
// is not a terminal, and without a "runspace:" prefix.
func TestLogDefaults(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.mustRun(t, createArgs()...)

	log := rig.stderr.String()
	requireContains(t, "stderr", log, createProgress)

	for _, unwanted := range []string{debugLevel, "runspace:", "\x1b["} {
		if strings.Contains(log, unwanted) {
			t.Errorf("stderr has %q: %s", unwanted, log)
		}
	}
}

// RUNSPACE_LOG_LEVEL sets the level, in any case, and -v/--verbose wins
// over it.
func TestLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level string
		// verbose is the verbose flag to pass, if any.
		verbose string
		info    bool
		debug   bool
	}{
		{level: "debug", verbose: "", info: true, debug: true},
		{level: "DEBUG", verbose: "", info: true, debug: true},
		{level: "info", verbose: "", info: true, debug: false},
		{level: "warn", verbose: "", info: false, debug: false},
		{level: "error", verbose: "", info: false, debug: false},
		{level: "error", verbose: "--" + verboseFlag, info: true, debug: true},
		{level: "", verbose: "-v", info: true, debug: true},
		// The flag wins, so RUNSPACE_LOG_LEVEL is not even read.
		{level: "loud", verbose: "-v", info: true, debug: true},
	}
	for _, test := range tests {
		t.Run(test.level+test.verbose, func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			rig.env[envLogLevel] = test.level

			args := createArgs()
			if test.verbose != "" {
				args = append(args, test.verbose)
			}

			rig.mustRun(t, args...)

			log := rig.stderr.String()
			if got := strings.Contains(log, infoLevel); got != test.info {
				t.Errorf("info lines: %t, want %t: %s", got, test.info, log)
			}

			if got := strings.Contains(log, debugLevel); got != test.debug {
				t.Errorf("debug lines: %t, want %t: %s", got, test.debug, log)
			}
		})
	}
}

// An unknown level fails before anything runs: exec exits 125 and other
// commands 1.
func TestInvalidLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args []string
		code int
	}{
		{[]string{cmdList}, 1},
		{[]string{cmdExec, ownedSandbox, "x"}, 125},
	}
	for _, test := range tests {
		t.Run(test.args[0], func(t *testing.T) {
			t.Parallel()

			rig := newHarness(t, nil)
			rig.env[envLogLevel] = "loud"

			if code := rig.run(test.args...); code != test.code {
				t.Errorf("exit %d, want %d", code, test.code)
			}

			requireContains(t, "stderr", rig.stderr.String(), errorLevel+`invalid RUNSPACE_LOG_LEVEL "loud": want debug`)

			if len(rig.gh.Calls()) != 0 {
				t.Errorf("GitHub was called: %v", rig.gh.Calls())
			}
		})
	}
}

// Over MCP, stdout carries the protocol, so the log file may not be stdout,
// as /dev/stdout would be. Here stdout is a file, and the log names its
// path.
func TestMCPRefusesLogOnStdout(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stdout")

	stdout, err := os.Create(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = stdout.Close() })

	rig := newHarness(t, nil)
	rig.app.stdout = stdout
	rig.env[envLogOutput] = path

	if code := rig.run("mcp"); code != 1 || len(rig.tokens) != 0 {
		t.Errorf("exit %d, want 1 without starting the server", code)
	}

	requireContains(t, "stderr", rig.stderr.String(), "RUNSPACE_LOG_OUTPUT="+path, "MCP protocol")
}

// A non-empty RUNSPACE_LOG_OUTPUT is a file that the log is appended to,
// readable only by its owner when runspace creates it.
func TestLogOutputFile(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("runspace holds the log file open until it exits, so Windows cannot remove the test's directory")
	}

	path := filepath.Join(t.TempDir(), "runspace.log")

	for range 2 {
		rig := newHarness(t, nil)
		rig.env[envLogOutput] = path
		rig.mustRun(t, createArgs()...)

		if rig.stderr.Len() != 0 {
			t.Errorf("stderr = %q, want nothing", rig.stderr.String())
		}
	}

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(string(data), createProgress); got != 2 {
		t.Errorf("the file has %d progress lines, want 2 appended: %s", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
}

// RUNSPACE_LOG_OUTPUT is always a path, so stdout and stderr name files in
// the current directory.
//
//nolint:paralleltest // t.Chdir changes the process-wide working directory
func TestLogOutputNamesAreFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runspace holds the log file open until it exits, so Windows cannot remove the test's directory")
	}

	t.Chdir(t.TempDir())

	for _, name := range []string{"stdout", "stderr"} {
		rig := newHarness(t, nil)
		rig.env[envLogOutput] = name
		rig.mustRun(t, createArgs()...)

		if rig.stdout.String() != "cs-2\n" || rig.stderr.Len() != 0 {
			t.Errorf("%s: stdout = %q, stderr = %q, want only the name on stdout",
				name, rig.stdout.String(), rig.stderr.String())
		}

		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}

		requireContains(t, "the file "+name, string(data), createProgress)
	}
}

// A log file that cannot be opened fails the command before it runs.
func TestLogOutputUnopenable(t *testing.T) {
	t.Parallel()

	rig := newHarness(t, nil)
	rig.env[envLogOutput] = filepath.Join(t.TempDir(), "missing", "runspace.log")

	if code := rig.run(createArgs()...); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}

	requireContains(t, "stderr", rig.stderr.String(), errorLevel+"RUNSPACE_LOG_OUTPUT: open ")

	if len(rig.gh.Calls()) != 0 {
		t.Errorf("GitHub was called: %v", rig.gh.Calls())
	}
}

// Debug lines name sandboxes and outcomes, never a token, an environment
// value, or the command line, which can hold secrets.
func TestDebugLogHoldsNoSecrets(t *testing.T) {
	t.Parallel()

	const (
		token    = "token-sentinel"
		envValue = "env-sentinel"
		argument = "argv-sentinel"
	)

	rig := newHarness(t, nil)
	rig.mustRun(t, "--"+verboseFlag, "-T", token, cmdExec, "-e", "SECRET="+envValue, ownedSandbox, "echo", argument)

	log := rig.stderr.String()
	requireContains(t, "stderr", log, debugLevel+"Running the command", debugLevel+"The command ended")

	for _, secret := range []string{token, envValue, argument} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds %q: %s", secret, log)
		}
	}
}
