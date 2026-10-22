//go:build live

package live

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// keptNotice starts the log line of a sandbox that run kept.
const keptNotice = "Kept the sandbox"

// --timeout in run deletes the sandbox, and --keep failure
// stops it; a later exec restarts it.
func TestRunTimeoutAndKeep(t *testing.T) {
	ownSandbox(t)
	r := runspace(t, "", nil, "run", "-R", liveRepo(), "--timeout", "1m", "sleep", "600")
	if r.code != 124 {
		t.Errorf("run --timeout: exit %d, want 124", r.code)
	}
	if strings.Contains(r.stderr, keptNotice) {
		t.Error("run --timeout kept its sandbox")
	}

	r = runspace(t, "", nil, "run", "-R", liveRepo(), "--keep", "failure", "sh", "-c", "exit 3")
	kept := logValue(r.stderr, keptNotice, "name")
	if r.code != 3 || kept == "" {
		t.Fatalf("run --keep failure: exit %d, %q", r.code, r.stderr)
	}
	cs, err := api(t).Get(context.Background(), kept)
	if err != nil {
		t.Fatal(err)
	}
	if cs.State != "Shutdown" && cs.State != "ShuttingDown" {
		t.Errorf("kept sandbox state = %s, want Shutdown", cs.State)
	}
	if r := runspace(t, "", nil, "exec", kept, "true"); r.code != 0 {
		t.Errorf("exec on a stopped sandbox: exit %d", r.code)
	}
	runspace(t, "", nil, "delete", kept)
}

// The keepalive holds a long command past a 5-minute idle timeout, and after
// runspace is killed the codespace stops within it. It waits out both, so
// -short skips it.
func TestIdleBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a 5-minute idle timeout twice")
	}
	ownSandbox(t)
	r := runspace(t, "", nil, "create", "-R", liveRepo(), "--idle", "5m")
	if r.code != 0 {
		t.Fatalf("create: %s", r.stderr)
	}
	name := strings.TrimSpace(r.stdout)
	defer runspace(t, "", nil, "delete", name)

	// An idle codespace has stopped about 5m40s after its last activity, so
	// a command that outlives 7 minutes was held by the keepalive.
	if r := runspace(t, "", nil, "exec", "--timeout", "10m", name, "sleep", "420"); r.code != 0 {
		t.Errorf("exec sleep 420 under --idle 5m: exit %d", r.code)
	}

	cmd := exec.Command(bin, "exec", name, "sleep", "600")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNSPACE_TOKEN=" + os.Getenv("RUNSPACE_TOKEN")}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
	cmd.Process.Kill()
	cmd.Wait()
	killed := time.Now()
	for time.Since(killed) < 15*time.Minute {
		cs, err := api(t).Get(context.Background(), name)
		if err == nil && cs.State == "Shutdown" {
			t.Logf("stopped %v after runspace was killed", time.Since(killed).Round(time.Second))
			return
		}
		time.Sleep(30 * time.Second)
	}
	t.Error("the codespace did not stop within 15 minutes of runspace being killed")
}
