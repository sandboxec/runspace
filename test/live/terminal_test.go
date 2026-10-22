//go:build live && unix

package live

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestInteractiveTerminal checks that exec -t gives a working terminal and
// that resizes propagate (Linux and macOS).
func TestInteractiveTerminal(t *testing.T) {
	name := shared(t)
	cmd := exec.Command(bin, "exec", "-t", name, "bash", "--norc", "--noprofile")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNSPACE_TOKEN=" + os.Getenv("RUNSPACE_TOKEN"), "TERM=xterm"}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 90})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var out syncBuffer
	go io.Copy(&out, ptmx)

	waitFor := func(s string) bool {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(out.String(), s) {
				return true
			}
			time.Sleep(200 * time.Millisecond)
		}
		return false
	}
	io.WriteString(ptmx, "echo size:$(stty size)\r")
	if !waitFor("size:30 90") {
		t.Errorf("initial size not seen; output:\n%s", out.String())
	}
	pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	cmd.Process.Signal(syscall.SIGWINCH)
	time.Sleep(time.Second)
	io.WriteString(ptmx, "echo size:$(stty size)\r")
	if !waitFor("size:40 120") {
		t.Errorf("resize not propagated; output:\n%s", out.String())
	}
	io.WriteString(ptmx, "exit 4\r")
	err = cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 4 {
		t.Errorf("exit = %v, want 4", err)
	}
}

// syncBuffer is a bytes.Buffer safe for one writer and one reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
