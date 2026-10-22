//go:build unix

package transfer_test

import (
	"bytes"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

// withUmask sets the process umask for the test. The umask is process-wide, so
// tests that use it cannot call t.Parallel: the test runner runs them before
// any parallel test resumes.
func withUmask(t *testing.T, mask int) {
	t.Helper()

	old := syscall.Umask(mask)

	t.Cleanup(func() { syscall.Umask(old) })
}

// TestExtractModes checks the permissions of extracted entries under two umasks.
//
//nolint:paralleltest // changes the process-wide umask, which cannot be isolated per test
func TestExtractModes(t *testing.T) {
	const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

	entries := []ent{
		dirWithMode("root", 0o1777),
		dirWithMode("root/d700", 0o700),
		dirWithMode("root/d2755", 0o2755),
		file("root/plain", "x", 0o600),
		file("root/exec", "x", 0o755),
		file("root/ownerexec", "x", 0o100),
		file("root/setuid", "x", 0o4755),
		file("root/setgid", "x", 0o2644),
		file("root/sticky", "x", 0o1644),
		file("root/all", "x", 0o7777),
		file("root/d700/f", "x", 0o400),
	}
	want := map[string]fs.FileMode{
		".": 0o755, "d700": 0o755, "d2755": 0o755,
		"plain": 0o644, "exec": 0o755, "ownerexec": 0o755,
		"setuid": 0o755, "setgid": 0o644, "sticky": 0o644, "all": 0o755,
		"d700/f": 0o644,
	}

	for _, testCase := range []struct {
		umask int
		clear fs.FileMode // permission bits removed by the umask
	}{{0o022, 0o022}, {0o077, 0o077}} {
		withUmask(t, testCase.umask)
		parent := mustMkdir(t, parentDir(t))
		dst := filepath.Join(parent, "out")

		mustCommit(t, mustExtract(t, entries, dst))

		for rel, mode := range want {
			info, err := os.Lstat(filepath.Join(dst, rel))
			if err != nil {
				t.Fatal(err)
			}

			if got, want := info.Mode().Perm(), mode&^testCase.clear; got != want {
				t.Errorf("umask %03o: %s mode = %o, want %o", testCase.umask, rel, got, want)
			}

			if info.Mode()&specialBits != 0 {
				t.Errorf("umask %03o: %s has special bits: %v", testCase.umask, rel, info.Mode())
			}
		}
	}
}

func TestWriteTarRefusesFIFO(t *testing.T) {
	t.Parallel()

	src := makeTree(t)

	fifo := filepath.Join(src, "pipe")

	err := syscall.Mkfifo(fifo, 0o644)
	if err != nil {
		t.Skip(err)
	}

	var buf bytes.Buffer

	_, err = transfer.WriteTar(&buf, src, "out", false)
	contains(t, err, fifo, "FIFO")
}

func TestWriteTarRefusesSocket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sock := filepath.Join(dir, "s")

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Skip(err) // for example, a temporary directory path too long for a socket
	}

	t.Cleanup(func() { _ = listener.Close() })

	_, err = transfer.WriteTar(&bytes.Buffer{}, dir, "out", false)
	contains(t, err, sock, "socket")
}

func TestCommitReplacesSymlink(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	target := filepath.Join(parent, "target")
	writeFile(t, target, "precious", 0o644)

	dst := filepath.Join(parent, "dst")

	err := os.Symlink(target, dst)
	if err != nil {
		t.Fatal(err)
	}

	mustCommit(t, mustExtract(t, []ent{file("f", "new", 0o644)}, dst))

	if got := readFile(t, target); got != "precious" {
		t.Errorf("symlink target written through: %q", got)
	}

	if info, _ := os.Lstat(dst); !info.Mode().IsRegular() {
		t.Errorf("dst mode = %v, want regular file", info.Mode())
	}
}

func TestExistingSymlinkToDirectoryIsNotMerged(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	realDir := mustMkdir(t, filepath.Join(parent, "real"))

	dst := filepath.Join(parent, "dst")

	err := os.Symlink(realDir, dst)
	if err != nil {
		t.Fatal(err)
	}

	mustCommit(t, mustExtract(t, []ent{file("f", "new", 0o644)}, dst))

	requireClean(t, realDir)
}
