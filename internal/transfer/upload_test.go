package transfer_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

func randomBody(t *testing.T, size int) string {
	t.Helper()

	body := make([]byte, size)

	_, err := rand.Read(body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// makeTree builds a small tree and returns its root.
func makeTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "src")
	writeFile(t, filepath.Join(root, "a.txt"), "alpha", 0o644)
	writeFile(t, filepath.Join(root, "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(root, "sub", "deeper", "big.bin"), randomBody(t, 300_000), 0o644)
	mustMkdir(t, filepath.Join(root, "empty"))

	return root
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()

	err := os.Symlink(target, link)
	if err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
}

// roundTrip uploads src as name and downloads it to dst.
func roundTrip(t *testing.T, src, name, dst string, follow bool) (transfer.Stats, transfer.Stats, error) {
	t.Helper()

	var buf bytes.Buffer

	uploaded, err := transfer.WriteTar(&buf, src, name, follow)
	if err != nil {
		return uploaded, transfer.Stats{}, fmt.Errorf("upload: %w", err)
	}

	staged, err := transfer.Extract(&buf, dst)
	if err != nil {
		return uploaded, transfer.Stats{}, fmt.Errorf("download: %w", err)
	}

	mustCommit(t, staged)

	return uploaded, staged.Stats(), nil
}

func TestWriteTarNames(t *testing.T) {
	t.Parallel()

	root := makeTree(t)

	var buf bytes.Buffer

	stats, err := transfer.WriteTar(&buf, root, "renamed", false)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]byte{
		"renamed/":                   tar.TypeDir,
		"renamed/a.txt":              tar.TypeReg,
		"renamed/bin/":               tar.TypeDir,
		"renamed/bin/run.sh":         tar.TypeReg,
		"renamed/empty/":             tar.TypeDir,
		"renamed/sub/":               tar.TypeDir,
		"renamed/sub/deeper/":        tar.TypeDir,
		"renamed/sub/deeper/big.bin": tar.TypeReg,
	}

	got := tarNames(t, buf.Bytes())
	if len(got) != len(want) {
		t.Errorf("entries = %v, want %v", got, want)
	}

	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("entry %q type = %q, want %q (all: %v)", name, got[name], typ, got)
		}
	}

	if stats != (transfer.Stats{Files: 3, Bytes: 5 + 10 + 300_000}) {
		t.Errorf("Stats = %+v", stats)
	}
}

func TestWriteTarHeaders(t *testing.T) {
	t.Parallel()

	root := makeTree(t)

	var buf bytes.Buffer

	_, err := transfer.WriteTar(&buf, root, "n", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, hdr := range tarHeaders(t, buf.Bytes()) {
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: ownership not cleared: %+v", hdr.Name, hdr)
		}

		if hdr.ModTime.IsZero() {
			t.Errorf("%s: no mtime", hdr.Name)
		}

		if hdr.Name == "n/bin/run.sh" && hdr.Mode&0o111 == 0 {
			t.Errorf("run.sh mode = %o, lost execute bit", hdr.Mode)
		}
	}
}

func TestWriteTarLongName(t *testing.T) {
	t.Parallel()

	name := string(bytes.Repeat([]byte("n"), 200))
	src := filepath.Join(t.TempDir(), "f")
	writeFile(t, src, "x", 0o644)

	var buf bytes.Buffer

	_, err := transfer.WriteTar(&buf, src, name, false)
	if err != nil {
		t.Fatal(err)
	}

	if got := tarNames(t, buf.Bytes()); got[name] != tar.TypeReg {
		t.Errorf("long name lost: %v", got)
	}
}

func TestWriteTarBadName(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "f")
	writeFile(t, src, "x", 0o644)

	for _, name := range []string{"", ".", "..", "a/b", "/a", "a/", "a\x00b"} {
		var buf bytes.Buffer

		_, err := transfer.WriteTar(&buf, src, name, false)
		if err == nil {
			t.Errorf("name %q accepted", name)
		}

		if buf.Len() != 0 {
			t.Errorf("name %q: wrote %d bytes before failing", name, buf.Len())
		}
	}
}

func TestWriteTarMissingSource(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "nope")
	_, err := transfer.WriteTar(io.Discard, missing, "n", false)
	contains(t, err, missing)
}

func TestRoundTripTree(t *testing.T) {
	t.Parallel()

	src := makeTree(t)
	dst := filepath.Join(mustMkdir(t, parentDir(t)), "out")

	uploaded, downloaded, err := roundTrip(t, src, "out", dst, false)
	if err != nil {
		t.Fatal(err)
	}

	want, got := hashTree(t, src), hashTree(t, dst)
	if len(want) != 3 || len(got) != len(want) {
		t.Fatalf("files: got %v want %v", got, want)
	}

	for name, sum := range want {
		if got[name] != sum {
			t.Errorf("%s: sha256 differs", name)
		}
	}

	if uploaded != downloaded || uploaded.Files != 3 {
		t.Errorf("stats: upload %+v, download %+v", uploaded, downloaded)
	}

	info, err := os.Stat(filepath.Join(dst, "empty"))
	if err != nil || !info.IsDir() {
		t.Errorf("empty dir lost: %v", err)
	}
}

func TestRoundTripFile(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "in.bin")
	body := randomBody(t, 100_000)
	writeFile(t, src, body, 0o644)
	dst := filepath.Join(mustMkdir(t, parentDir(t)), "out.bin")

	uploaded, downloaded, err := roundTrip(t, src, "out.bin", dst, false)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dst); got != body {
		t.Error("content differs")
	}

	if want := (transfer.Stats{Files: 1, Bytes: 100_000}); uploaded != want || downloaded != want {
		t.Errorf("stats: upload %+v, download %+v, want %+v", uploaded, downloaded, want)
	}
}

func TestSymlinkKeptThenRefused(t *testing.T) {
	t.Parallel()

	src := makeTree(t)
	symlinkOrSkip(t, "a.txt", filepath.Join(src, "link"))
	symlinkOrSkip(t, "/does/not/exist", filepath.Join(src, "dangling"))

	var buf bytes.Buffer

	_, err := transfer.WriteTar(&buf, src, "out", false)
	if err != nil {
		t.Fatal(err)
	}

	links := map[string]string{}

	for _, hdr := range tarHeaders(t, buf.Bytes()) {
		if hdr.Typeflag == tar.TypeSymlink {
			links[hdr.Name] = hdr.Linkname
		}
	}

	if links["out/link"] != "a.txt" || links["out/dangling"] != "/does/not/exist" {
		t.Errorf("symlinks = %v", links)
	}

	parent := mustMkdir(t, parentDir(t))
	_, err = transfer.Extract(&buf, filepath.Join(parent, "out"))
	contains(t, err, `"out/dangling"`, symlinkWord, "symbolic links")
	requireClean(t, parent)
}

func TestSymlinkSourceKeptThenRefused(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "t")
	writeFile(t, target, "x", 0o644)
	link := filepath.Join(t.TempDir(), "l")
	symlinkOrSkip(t, target, link)

	var buf bytes.Buffer

	_, err := transfer.WriteTar(&buf, link, "l", false)
	if err != nil {
		t.Fatal(err)
	}

	if got := tarNames(t, buf.Bytes()); got["l"] != tar.TypeSymlink {
		t.Errorf("entries = %v", got)
	}
}

func TestFollow(t *testing.T) {
	t.Parallel()

	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "o.txt"), "outside file", 0o644)
	writeFile(t, filepath.Join(outside, "nested", "n.txt"), "nested", 0o644)

	src := makeTree(t)
	symlinkOrSkip(t, filepath.Join(src, "a.txt"), filepath.Join(src, "filelink"))
	symlinkOrSkip(t, outside, filepath.Join(src, "dirlink"))

	dst := filepath.Join(mustMkdir(t, parentDir(t)), "out")

	uploaded, downloaded, err := roundTrip(t, src, "out", dst, true)
	if err != nil {
		t.Fatal(err)
	}

	for rel, want := range map[string]string{
		"filelink":             "alpha",
		"dirlink/o.txt":        "outside file",
		"dirlink/nested/n.txt": "nested",
	} {
		followed := filepath.Join(dst, filepath.FromSlash(rel))

		if got := readFile(t, followed); got != want {
			t.Errorf("%s = %q; want %q", rel, got, want)
		}

		info, err := os.Lstat(followed)
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s is not a regular file", rel)
		}
	}

	if uploaded != downloaded || uploaded.Files != 6 {
		t.Errorf("stats: upload %+v, download %+v, want 6 files", uploaded, downloaded)
	}
}

func TestFollowSourceSymlink(t *testing.T) {
	t.Parallel()

	realDir := filepath.Join(t.TempDir(), "real")
	writeFile(t, filepath.Join(realDir, "f"), "content", 0o644)
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, realDir, link)

	dst := filepath.Join(mustMkdir(t, parentDir(t)), "out")

	_, _, err := roundTrip(t, link, "out", dst, true)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(dst, "f")); got != "content" {
		t.Errorf("f = %q", got)
	}
}

func TestFollowDirectoryLoop(t *testing.T) {
	t.Parallel()

	src := makeTree(t)
	loop := filepath.Join(src, "sub", "loop")
	symlinkOrSkip(t, "..", loop)

	var buf bytes.Buffer

	_, err := transfer.WriteTar(&buf, src, "out", true)
	contains(t, err, loop, "loop")

	_, err = transfer.Extract(&buf, filepath.Join(mustMkdir(t, parentDir(t)), "out"))
	if err == nil {
		t.Error("truncated stream after loop error was accepted")
	}

	// Without -L the same link is just a symlink.
	_, err = transfer.WriteTar(io.Discard, src, "out", false)
	if err != nil {
		t.Errorf("no-follow: %v", err)
	}
}

func TestFollowDanglingSymlink(t *testing.T) {
	t.Parallel()

	src := makeTree(t)
	dangling := filepath.Join(src, "dangling")
	symlinkOrSkip(t, "nowhere", dangling)

	_, err := transfer.WriteTar(io.Discard, src, "out", true)
	contains(t, err, dangling)
}

func BenchmarkRoundTrip(b *testing.B) {
	const size = 64 << 20

	src := filepath.Join(b.TempDir(), "big.bin")

	file, err := os.Create(filepath.Clean(src))
	if err != nil {
		b.Fatal(err)
	}

	chunk := bytes.Repeat([]byte{0xa5}, 1<<20)
	for range size / len(chunk) {
		_, err = file.Write(chunk)
		if err != nil {
			b.Fatal(err)
		}
	}

	err = file.Close()
	if err != nil {
		b.Fatal(err)
	}

	parent := b.TempDir()
	dst := filepath.Join(parent, "out.bin")

	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()

	for range b.N {
		reader, writer := io.Pipe()

		go func() {
			_, err := transfer.WriteTar(writer, src, "out.bin", false)
			_ = writer.CloseWithError(err) // always returns nil
		}()

		staged, err := transfer.Extract(reader, dst)
		if err != nil {
			_ = reader.CloseWithError(err) // always returns nil

			b.Fatal(err)
		}

		err = staged.Commit()
		if err != nil {
			b.Fatal(err)
		}
	}
}
