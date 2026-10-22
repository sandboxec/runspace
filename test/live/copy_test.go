//go:build live

package live

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// cp round trips, refuses links unless -L, refuses an existing directory DST,
// and streams a listable tar for NAME:SRC -.
func TestCopyRoundTrip(t *testing.T) {
	name := shared(t)
	src := t.TempDir()
	files := map[string]string{"a.txt": "alpha\n", "sub/b.txt": "beta\n", "sub/deep/c.bin": strings.Repeat("\x00\xff", 4096)}
	for p, body := range files {
		os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(src, p), []byte(body), 0o644)
	}
	os.Chmod(filepath.Join(src, "a.txt"), 0o755)

	if r := runspace(t, "", nil, "cp", src, name+":tree"); r.code != 0 {
		t.Fatalf("upload: %s", r.stderr)
	}
	back := filepath.Join(t.TempDir(), "tree")
	if r := runspace(t, "", nil, "cp", name+":tree", back); r.code != 0 {
		t.Fatalf("download: %s", r.stderr)
	}
	if got, want := sums(t, back), sums(t, src); !mapsEqual(got, want) {
		t.Errorf("round trip changed content:\n got %v\nwant %v", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(back, "a.txt")); runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("a.txt lost its execute bit: %v", fi.Mode())
	}

	if r := runspace(t, "", nil, "exec", name, "sh", "-c", "mkdir -p linked && echo hi > target.txt && ln -sf ../target.txt linked/link"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	dst := filepath.Join(t.TempDir(), "linked")
	r := runspace(t, "", nil, "cp", name+":linked", dst)
	if r.code != 1 || !strings.Contains(r.stderr, "-L") {
		t.Errorf("symlink without -L: exit %d, %q", r.code, r.stderr)
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Error("a refused download left its DST")
	}
	if r := runspace(t, "", nil, "cp", "-L", name+":linked", dst); r.code != 0 {
		t.Fatalf("symlink with -L: %s", r.stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "link")); string(b) != "hi\n" {
		t.Errorf("-L copied %q", b)
	}

	if r := runspace(t, "", nil, "cp", name+":tree", back); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Errorf("existing directory DST: exit %d, %q", r.code, r.stderr)
	}

	r = runspace(t, "", nil, "cp", name+":tree", "-")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	tr := tar.NewReader(strings.NewReader(r.stdout))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("listing the tar stream: %v", err)
		}
		names = append(names, h.Name)
	}
	if !bytes.Contains([]byte(strings.Join(names, " ")), []byte("tree/sub/deep/c.bin")) {
		t.Errorf("tar stream lists %q", names)
	}
}

// cp throughput over the tunnel, for the record: 64 MiB each way.
func TestCopyThroughput(t *testing.T) {
	name := shared(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	data := make([]byte, 64<<20)
	rand.Read(data)
	os.WriteFile(src, data, 0o644)

	up := runspace(t, "", nil, "cp", src, name+":big.bin")
	if up.code != 0 {
		t.Fatal(up.stderr)
	}
	back := filepath.Join(dir, "back.bin")
	down := runspace(t, "", nil, "cp", name+":big.bin", back)
	if down.code != 0 {
		t.Fatal(down.stderr)
	}
	got, _ := os.ReadFile(back)
	if sha256.Sum256(got) != sha256.Sum256(data) || !bytes.Equal(got[:16], data[:16]) {
		t.Fatal("64 MiB round trip changed the content")
	}
	mib := func(d time.Duration) float64 { return 64 / d.Seconds() }
	t.Logf("cp 64 MiB: upload %v (%.1f MiB/s incl. dial), download %v (%.1f MiB/s incl. dial)",
		up.took.Round(time.Millisecond), mib(up.took), down.took.Round(time.Millisecond), mib(down.took))
}

func sums(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		s := sha256.Sum256(b)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(s[:])
		return nil
	})
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
