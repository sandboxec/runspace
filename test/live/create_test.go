//go:build live

package live

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A repository without a devcontainer config gets SSH and git without setup,
// and a default create sets the runspace tag, idle 10, and retention 1440.
func TestDefaultCreate(t *testing.T) {
	name := shared(t)
	cs, err := api(t).Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if cs.DisplayName != "runspace" {
		t.Errorf("display name = %q", cs.DisplayName)
	}
	if cs.IdleTimeoutMinutes == nil || *cs.IdleTimeoutMinutes != 10 {
		t.Errorf("idle timeout = %v, want 10", deref(cs.IdleTimeoutMinutes))
	}
	if cs.RetentionPeriodMinutes == nil || *cs.RetentionPeriodMinutes != 1440 {
		t.Errorf("retention = %v, want 1440", deref(cs.RetentionPeriodMinutes))
	}
	if cs.Machine == nil || cs.Machine.Name != "basicLinux32gb" {
		t.Errorf("machine = %+v", cs.Machine)
	}
	r := runspace(t, "", nil, "exec", name, "git", "rev-parse", "refs/runspace/base")
	if r.code != 0 || len(strings.TrimSpace(r.stdout)) != 40 {
		t.Errorf("base ref: exit %d, %q", r.code, r.stdout)
	}
}

// A devcontainer without an SSH server fails with the sshd hint, and the
// half-made sandbox is deleted.
func TestNoSSHServer(t *testing.T) {
	ownSandbox(t)
	r := runspace(t, "", nil, "create", "--verbose", "-R", liveRepo(), "-b", "no-sshd")
	if r.code != 1 || !strings.Contains(r.stderr, "ghcr.io/devcontainers/features/sshd:1") {
		t.Errorf("exit %d, stderr %q; want 1 and the sshd hint", r.code, r.stderr)
	}
	// Other checks create sandboxes at the same time, so look up this one by
	// the name that the debug log gives.
	name := logValue(r.stderr, "Created the codespace", "name")
	if name == "" {
		t.Fatal("create did not log the name of the codespace it made")
	}
	if cs, err := api(t).Get(context.Background(), name); err == nil && cs.State == "Available" {
		t.Errorf("the half-made sandbox %s is still available", name)
	}
}

// Untagged codespaces are refused, and ls lists only tagged codespaces.
func TestRefusesUntagged(t *testing.T) {
	tagged := shared(t)

	out, err := exec.Command("gh", "codespace", "create", "-R", liveRepo(), "-m", "basicLinux32gb",
		"--idle-timeout", "5m", "--retention-period", "1h", "--display-name", "not-runspace", "--default-permissions").Output()
	if err != nil {
		// Output keeps gh's stderr in the error, which says why it failed.
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("gh codespace create: %v\n%s", err, indent(string(stderr)))
	}
	untagged := strings.TrimSpace(string(out))
	t.Logf("untagged codespace %s", untagged)
	// runspace refuses to delete it, and a running one takes up a place
	// under GitHub's limit while the parallel checks run.
	t.Cleanup(func() {
		if out, err := exec.Command("gh", "codespace", "delete", "-c", untagged, "--force").CombinedOutput(); err != nil {
			t.Logf("delete %s: %v\n%s", untagged, err, indent(string(out)))
		}
	})

	for _, args := range [][]string{
		{"exec", untagged, "true"},
		{"diff", untagged},
		{"cp", untagged + ":README.md", t.TempDir() + "/x"},
		{"delete", untagged},
	} {
		r := runspace(t, "", nil, args...)
		if r.code == 0 || !strings.Contains(r.stderr, "not created by runspace") {
			t.Errorf("%q: exit %d, %q", args, r.code, r.stderr)
		}
	}
	ls := runspace(t, "", nil, "ls", "-q")
	if strings.Contains(ls.stdout, untagged) || !strings.Contains(ls.stdout, tagged) {
		t.Errorf("ls -q = %q; want %s and not %s", ls.stdout, tagged, untagged)
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
