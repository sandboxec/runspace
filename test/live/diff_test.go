//go:build live

package live

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// diff covers a remote commit and an uncommitted edit, ignores the
// sandbox's git configuration, and leaves the index alone.
func TestDiff(t *testing.T) {
	name := shared(t)
	setup := `set -e
git config color.ui always
git config diff.noprefix true
git config diff.mnemonicPrefix true
echo committed >> README.md
git -c user.name=t -c user.email=t@example.com commit -qam "remote commit"
echo uncommitted >> README.md
printf 'new file\n' > added.txt
head -c 300 /dev/urandom > blob.bin`
	if r := runspace(t, "", nil, "exec", name, "sh", "-c", setup); r.code != 0 {
		t.Fatalf("setup: exit %d: %s", r.code, r.stderr)
	}
	index := func() string {
		return runspace(t, "", nil, "exec", name, "sh", "-c", "git ls-files -s --debug | sha256sum").stdout
	}
	before := index()
	out := filepath.Join(t.TempDir(), "task.patch")
	if r := runspace(t, "", nil, "diff", "-o", out, name); r.code != 0 {
		t.Fatalf("diff: exit %d: %s", r.code, r.stderr)
	}
	if after := index(); after != before {
		t.Errorf("remote index changed: %q → %q", before, after)
	}
	patch, _ := os.ReadFile(out)
	if bytes.Contains(patch, []byte("\x1b[")) || !bytes.Contains(patch, []byte("--- a/README.md")) || !bytes.Contains(patch, []byte("GIT binary patch")) {
		t.Errorf("patch looks wrong:\n%s", patch)
	}

	base := strings.TrimSpace(runspace(t, "", nil, "exec", name, "git", "rev-parse", "refs/runspace/base").stdout)
	dir := t.TempDir()
	if err := exec.Command("gh", "repo", "clone", liveRepo(), dir, "--", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", dir, "checkout", "-q", base).Run()
	if out, err := exec.Command("git", "-C", dir, "apply", "--check", out).CombinedOutput(); err != nil {
		t.Errorf("git apply --check: %v\n%s", err, out)
	}
}
