//go:build unix

//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// logChildEnv makes the test binary log one line and exit, as the child of
// TestLogOnTerminal.
const logChildEnv = "RUNSPACE_TEST_LOG_CHILD"

// On a terminal the log has colors unless NO_COLOR has any value, as
// https://no-color.org asks. The logger never queries the terminal for its
// colors: nothing answers on this pty, so a query would stall for seconds
// and show in the output. The child runs with the pty as its controlling
// terminal, as a query goes only to that terminal.
func TestLogOnTerminal(t *testing.T) {
	t.Parallel()

	if os.Getenv(logChildEnv) != "" {
		newLogger(os.Stderr).Error("boom")

		return
	}

	tests := []struct {
		noColor string
		color   bool
	}{
		{noColor: "", color: true},
		{noColor: "yes", color: false},
	}
	for _, test := range tests {
		t.Run("NO_COLOR="+test.noColor, func(t *testing.T) {
			t.Parallel()

			out := logOnTerminal(t, "NO_COLOR="+test.noColor)
			// The level's 256-color foreground; bold may stay without colors.
			if got := strings.Contains(out, "38;5;"); got != test.color {
				t.Errorf("colors: %t, want %t: %q", got, test.color, out)
			}
		})
	}
}

// logOnTerminal runs this test's child on a pty with env added and returns
// what it printed there. It fails the test if the child took long or
// queried the terminal.
func logOnTerminal(t *testing.T, env string) string {
	t.Helper()

	//nolint:gosec // G204: the test binary runs itself, as this test's child.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLogOnTerminal$")
	// A 256-color terminal outside tmux, whatever runs the test.
	child.Env = append(os.Environ(), logChildEnv+"=1", "TERM=xterm-256color",
		"COLORTERM=", "TMUX=", "CLICOLOR=", "CLICOLOR_FORCE=", env)

	start := time.Now()

	ptmx, err := pty.Start(child)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}

	defer func() { _ = ptmx.Close() }()

	out := readUntil(t, ptmx, "boom")

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("logging took %v", took)
	}

	_ = child.Wait()

	if !strings.Contains(out, "ERROR") {
		t.Errorf("no level: %q", out)
	}

	for _, query := range []string{"\x1b]10;?", "\x1b]11;?", "\x1b[6n"} {
		if strings.Contains(out, query) {
			t.Errorf("the logger queried the terminal with %q: %q", query, out)
		}
	}

	return out
}

// readUntil reads reader until the output holds want, or fails after a few
// seconds.
func readUntil(t *testing.T, reader io.Reader, want string) string {
	t.Helper()

	var out bytes.Buffer

	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, 256)
		for !strings.Contains(out.String(), want) {
			n, err := reader.Read(buf)
			out.Write(buf[:n])

			if err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no log line on the terminal")
	}

	return out.String()
}
