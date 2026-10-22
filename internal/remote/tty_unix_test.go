//go:build unix

//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/creack/pty"
)

// With a TTY, a pty of the terminal's size is requested.
func TestPtyRequestedWithTTY(t *testing.T) {
	t.Parallel()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}

	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()

	err = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 100, X: 0, Y: 0})
	if err != nil {
		t.Fatal(err)
	}

	server := newTestServer()
	conn := dialTest(t, server)

	var out bytes.Buffer

	cmd := testCmd(behaviorPty)
	cmd.Stdout = &out
	cmd.TTY = tty

	code, err := conn.Exec(context.Background(), cmd)
	if code != 0 || err != nil || out.String() != behaviorPty {
		t.Fatalf("got %d, %q, %v", code, out.String(), err)
	}

	if got := server.ptySize.Load(); got != "100x40" {
		t.Errorf("pty size = %v, want 100x40", got)
	}
}
