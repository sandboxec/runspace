//go:build live

package live

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// Exit codes.
func TestExitCodes(t *testing.T) {
	name := shared(t)
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"exec", name, "sh", "-c", "exit 7"}, 7},
		{[]string{"exec", name, "sh", "-c", "kill -9 $$"}, 137},
		{[]string{"exec", name, "no-such-cmd"}, 127},
		{[]string{"exec", "-w", "/nonexistent", name, "true"}, 125},
	} {
		if r := runspace(t, "", nil, tt.args...); r.code != tt.code {
			t.Errorf("%q: exit %d, want %d", tt.args, r.code, tt.code)
		}
	}
}

// --timeout expiry terminates the remote command.
func TestTimeoutTerminatesRemoteCommand(t *testing.T) {
	name := shared(t)
	r := runspace(t, "", nil, "exec", "--timeout", "5s", name, "sleep", "600")
	if r.code != 124 {
		t.Errorf("timeout: exit %d, want 124", r.code)
	}
	if r.took > 30*time.Second {
		t.Errorf("timeout took %v", r.took)
	}
	time.Sleep(2 * time.Second)
	// Match the full command line: the container's init runs its own
	// "sleep 1" loop.
	if r := runspace(t, "", nil, "exec", name, "pgrep", "-f", "^sleep 600$"); r.code != 1 {
		t.Errorf("pgrep after timeout: exit %d, output %q; the remote sleep survived", r.code, r.stdout)
	}
	v := runspace(t, "", nil, "exec", name, "sh", "-c", "ssh -V 2>&1")
	t.Logf("ssh version in the default image: %s", strings.TrimSpace(v.stdout))
}

// Quoting and streams.
func TestQuotingAndStreams(t *testing.T) {
	name := shared(t)
	r := runspace(t, "", nil, "exec", name, "printf", `%s\n`, "a b", "it's", "$HOME")
	if r.code != 0 || r.stdout != "a b\nit's\n$HOME\n" {
		t.Errorf("printf: exit %d, %q", r.code, r.stdout)
	}
	if r := runspace(t, "piped", nil, "exec", "-i", name, "cat"); r.stdout != "piped" {
		t.Errorf("-i: %q", r.stdout)
	}
	if r := runspace(t, "ignored", nil, "exec", name, "cat"); r.code != 0 || r.stdout != "" || r.took > time.Minute {
		t.Errorf("no -i: exit %d, %q in %v", r.code, r.stdout, r.took)
	}

	data := make([]byte, 1<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	r = runspace(t, string(data), nil, "exec", "-i", name, "sha256sum")
	if got, _, _ := strings.Cut(r.stdout, " "); got != want {
		t.Errorf("upload sha256 = %q, want %q", got, want)
	}
	r = runspace(t, string(data), nil, "exec", "-i", name, "cat")
	if got := sha256.Sum256([]byte(r.stdout)); hex.EncodeToString(got[:]) != want {
		t.Errorf("round-trip sha256 differs; got %d bytes", len(r.stdout))
	}
}
