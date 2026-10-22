package transfer_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

const (
	symlinkWord = "symlink"
	rootA       = "root/a"
)

var errConnectionReset = errors.New("connection reset")

// refusal is an archive that Extract must refuse, and the error it must give.
type refusal struct {
	name    string
	entries []ent
	want    []string // substrings of the error
}

func refusals() []refusal {
	const elsewhere = "/tmp/elsewhere" // never written; the archive is refused first

	return []refusal{
		{"dotdot first", []ent{file("../x", "x", 0o644)}, []string{`"../x"`}},
		{"dotdot nested", []ent{dir("root"), file("root/../../x", "x", 0o644)}, []string{`"root/../../x"`}},
		{"absolute", []ent{file("/etc/x", "x", 0o644)}, []string{`"/etc/x"`}},
		{"empty name", []ent{file("", "x", 0o644)}, []string{"empty"}},
		{"dot", []ent{dir("./")}, []string{`"./"`}},
		{"second root", []ent{dir("root"), file("other/x", "x", 0o644)}, []string{`"other/x"`, `"root"`}},
		{
			"symlink",
			[]ent{dir("root"), special("root/l", tar.TypeSymlink, "f")},
			[]string{`"root/l"`, symlinkWord, "symbolic links"},
		},
		{"symlink write-through", []ent{
			dir("root"),
			special(rootA, tar.TypeSymlink, elsewhere),
			file("root/a/f", "pwned", 0o644),
		}, []string{`"root/a"`, symlinkWord, "symbolic links"}},
		{
			"hardlink",
			[]ent{dir("root"), file("root/f", "x", 0o644), special("root/h", tar.TypeLink, "root/f")},
			[]string{`"root/h"`, "hardlink", "hard links"},
		},
		{
			"char device",
			[]ent{dir("root"), special("root/c", tar.TypeChar, "")},
			[]string{`"root/c"`, "character device"},
		},
		{
			"block device",
			[]ent{dir("root"), special("root/b", tar.TypeBlock, "")},
			[]string{`"root/b"`, "block device"},
		},
		{"fifo", []ent{dir("root"), special("root/p", tar.TypeFifo, "")}, []string{`"root/p"`, "FIFO"}},
		{"dir over file", []ent{file("root", "x", 0o644), file("root/f", "x", 0o644)}, []string{`"root/f"`}},
		{"file over dir", []ent{dir("root"), dir("root/d"), file("root/d", "x", 0o644)}, []string{`"root/d"`}},
		{"empty archive", nil, []string{"no entries"}},
	}
}

func TestExtractRefusals(t *testing.T) {
	t.Parallel()

	for _, testCase := range refusals() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			parent := mustMkdir(t, filepath.Join(base, "parent"))
			outside := mustMkdir(t, filepath.Join(base, "outside"))
			data := buildTar(testCase.entries)
			dst := filepath.Join(parent, "dst")

			staged, err := transfer.Extract(bytes.NewReader(data), dst)
			if err == nil {
				abort(t, staged)
			}

			contains(t, err, testCase.want...)

			_, err = os.Lstat(dst)
			if !os.IsNotExist(err) {
				t.Errorf("dst exists after refusal: %v", err)
			}

			requireClean(t, parent)
			requireClean(t, outside)

			if entries, _ := os.ReadDir(base); len(entries) != 2 {
				t.Errorf("base has %d entries, want 2", len(entries))
			}
		})
	}
}

func TestExtractWriteThroughTargetUntouched(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	parent := mustMkdir(t, filepath.Join(base, "parent"))
	outside := mustMkdir(t, filepath.Join(base, "outside"))
	data := buildTar([]ent{
		dir("root"),
		special(rootA, tar.TypeSymlink, outside),
		file("root/a/f", "pwned", 0o644),
	})
	_, err := transfer.Extract(bytes.NewReader(data), filepath.Join(parent, "dst"))
	contains(t, err, `"root/a"`, symlinkWord)
	requireClean(t, outside)
	requireClean(t, parent)
}

func TestExtractSingleFile(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "out.txt")

	staged := mustExtract(t, []ent{file("src.txt", "hello", 0o600)}, dst)

	if got := staged.Stats(); got != (transfer.Stats{Files: 1, Bytes: 5}) {
		t.Errorf("Stats = %+v", got)
	}

	_, err := os.Lstat(dst)
	if !os.IsNotExist(err) {
		t.Errorf("dst exists before Commit: %v", err)
	}

	mustCommit(t, staged)

	if got := readFile(t, dst); got != "hello" {
		t.Errorf("content = %q", got)
	}

	if left := tempLeftovers(t, parent); len(left) != 0 {
		t.Errorf("temp leftovers: %v", left)
	}
}

func TestExtractTree(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "out")
	staged := mustExtract(t, []ent{
		file("root/deep/er/f.txt", "implicit parents", 0o644), // before its dirs
		dir("root/"),
		dir("root/empty/"),
		file("./root/a.txt", "first", 0o644),
		file("root/a.txt", "second", 0o644), // overwrites
	}, dst)
	mustCommit(t, staged)

	if got := readFile(t, filepath.Join(dst, "a.txt")); got != "second" {
		t.Errorf("a.txt = %q, want overwritten", got)
	}

	if got := readFile(t, filepath.Join(dst, "deep", "er", "f.txt")); got != "implicit parents" {
		t.Errorf("f.txt = %q", got)
	}

	info, err := os.Stat(filepath.Join(dst, "empty"))
	if err != nil || !info.IsDir() {
		t.Errorf("empty dir missing: %v", err)
	}
}

func TestExtractParentMustExist(t *testing.T) {
	t.Parallel()

	dst := filepath.Join(t.TempDir(), "missing", "dst")

	_, err := transfer.Extract(bytes.NewReader(buildTar([]ent{file("f", "x", 0o644)})), dst)
	if err == nil {
		t.Fatal("want error")
	}

	_, err = os.Stat(filepath.Dir(dst))
	if !os.IsNotExist(err) {
		t.Errorf("parent was created: %v", err)
	}
}

func TestTruncatedStream(t *testing.T) {
	t.Parallel()

	full := buildTar([]ent{
		dir("root"),
		file("root/big", strings.Repeat("abcdefgh", 1000), 0o644),
		file("root/z", "z", 0o644),
	})
	// Offsets: dir header 0-511, big header 512-1023, data from 1024.
	cuts := map[string]int{
		"mid file":              1024 + 3000,
		"mid header":            512 + 100,
		"mid first header":      200,
		"between entries":       1024 + 8192, // after big's data, before z's header
		"no end-of-archive":     len(full) - transfer.TrailerSize,
		"half end-of-archive":   len(full) - 512,
		"just first header":     512,
		"missing second header": 1024 + 8000 + 100,
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			parent := mustMkdir(t, parentDir(t))
			dst := filepath.Join(parent, "dst")

			staged, err := transfer.Extract(bytes.NewReader(full[:cut]), dst)
			if err == nil {
				abort(t, staged)
				t.Fatal("want error")
			}

			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("error %v does not wrap io.ErrUnexpectedEOF", err)
			}

			_, err = os.Lstat(dst)
			if !os.IsNotExist(err) {
				t.Errorf("dst exists: %v", err)
			}

			requireClean(t, parent)
		})
	}
}

// A file whose content ends in at least trailerSize zero bytes must not make
// a stream cut before the next header pass for a complete archive.
func TestTruncatedAfterZeroContent(t *testing.T) {
	t.Parallel()

	full := buildTar([]ent{
		dir("root"),
		file("root/zeros", strings.Repeat("\x00", 2*512), 0o644),
		file("root/z", "z", 0o644),
	})
	// Offsets: dir header 0-511, zeros header 512-1023, data 1024-2047.
	cut := 1024 + 2*512
	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")

	staged, err := transfer.Extract(bytes.NewReader(full[:cut]), dst)
	if err == nil {
		abort(t, staged)
		t.Fatal("want error")
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "missing end-of-archive marker") {
		t.Errorf("error %v is not the missing end-of-archive marker", err)
	}

	requireClean(t, parent)
}

func TestReadErrorPropagates(t *testing.T) {
	t.Parallel()

	full := buildTar([]ent{file("f", strings.Repeat("x", 5000), 0o644)})
	parent := mustMkdir(t, parentDir(t))
	archive := io.MultiReader(bytes.NewReader(full[:2000]), errReader{errConnectionReset})

	_, err := transfer.Extract(archive, filepath.Join(parent, "dst"))
	if !errors.Is(err, errConnectionReset) {
		t.Errorf("error %v does not wrap the read error", err)
	}

	requireClean(t, parent)
}

// A stream error that wraps io.EOF, such as the "start command: EOF" of a
// channel that closed early, is a failure, not the end of the archive.
func TestExtractWrappedEOF(t *testing.T) {
	t.Parallel()

	lost := fmt.Errorf("start command: %w", io.EOF)
	parent := mustMkdir(t, parentDir(t))

	_, err := transfer.Extract(errReader{lost}, filepath.Join(parent, "dst"))
	if !errors.Is(err, lost) {
		t.Errorf("error %v does not wrap the stream error", err)
	}

	requireClean(t, parent)
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestAbort(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")

	staged := mustExtract(t, []ent{file("f", "x", 0o644)}, dst)

	if left := tempLeftovers(t, parent); len(left) != 1 {
		t.Fatalf("temp dirs before Abort = %v, want one", left)
	}

	abort(t, staged)

	err := staged.Abort()
	if err != nil {
		t.Errorf("second Abort: %v", err)
	}

	err = staged.Commit()
	if err == nil {
		t.Error("Commit after Abort succeeded")
	}

	requireClean(t, parent)
}

func TestAbortAfterCommit(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")

	staged := mustExtract(t, []ent{file("f", "x", 0o644)}, dst)
	mustCommit(t, staged)

	err := staged.Abort()
	if err != nil {
		t.Errorf("Abort after Commit: %v", err)
	}

	if got := readFile(t, dst); got != "x" {
		t.Errorf("Abort after Commit damaged dst: %q", got)
	}

	err = staged.Commit()
	if err == nil {
		t.Error("second Commit succeeded")
	}
}

// failReader fails the test if it is read.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("stream read despite existing directory dst")

	return 0, io.EOF
}

func TestExistingDirectoryRefused(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := mustMkdir(t, filepath.Join(parent, "dst"))
	writeFile(t, filepath.Join(dst, "keep"), "keep", 0o644)

	_, err := transfer.Extract(failReader{t}, dst)
	contains(t, err, dst, "already exists and is a directory")

	if got := readFile(t, filepath.Join(dst, "keep")); got != "keep" {
		t.Error("existing directory modified")
	}

	if left := tempLeftovers(t, parent); len(left) != 0 {
		t.Errorf("temp leftovers: %v", left)
	}
}

func TestCommitRechecksDirectory(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")

	staged := mustExtract(t, []ent{file("f", "x", 0o644)}, dst)

	mustMkdir(t, dst) // appears after Extract
	contains(t, staged.Commit(), "already exists and is a directory")

	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("directory dst modified: %v", entries)
	}

	if left := tempLeftovers(t, parent); len(left) != 0 {
		t.Errorf("temp leftovers: %v", left)
	}
}

func TestCommitReplacesFile(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")
	writeFile(t, dst, "old", 0o600)

	staged := mustExtract(t, []ent{file("f", "new", 0o644)}, dst)

	if got := readFile(t, dst); got != "old" {
		t.Errorf("dst changed before Commit: %q", got)
	}

	mustCommit(t, staged)

	if got := readFile(t, dst); got != "new" {
		t.Errorf("dst = %q, want new", got)
	}

	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		t.Errorf("parent has %d entries, want only dst", len(entries))
	}
}

func TestCommitDirectoryOverFileFails(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")
	writeFile(t, dst, "old", 0o644)

	staged := mustExtract(t, []ent{dir("d"), file("d/f", "x", 0o644)}, dst)

	contains(t, staged.Commit(), dst, "not a directory")

	if got := readFile(t, dst); got != "old" {
		t.Errorf("dst changed: %q", got)
	}

	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		t.Errorf("parent has %d entries, want only dst", len(entries))
	}
}

func TestCommitDirectory(t *testing.T) {
	t.Parallel()

	parent := mustMkdir(t, parentDir(t))
	dst := filepath.Join(parent, "dst")

	staged := mustExtract(t, []ent{dir("d"), file("d/f", "x", 0o644)}, dst)

	if got := staged.Stats(); got != (transfer.Stats{Files: 1, Bytes: 1}) {
		t.Errorf("Stats = %+v", got)
	}

	mustCommit(t, staged)

	if got := readFile(t, filepath.Join(dst, "f")); got != "x" {
		t.Errorf("f = %q", got)
	}

	if left := tempLeftovers(t, parent); len(left) != 0 {
		t.Errorf("temp leftovers: %v", left)
	}
}

// FuzzExtract checks that no archive writes outside the temp dir or leaves
// one behind on error.
func FuzzExtract(f *testing.F) {
	f.Add(buildTar([]ent{dir("root"), file("root/a", "a", 0o755), dir("root/d"), file("root/d/b", "b", 0o644)}))
	f.Add(buildTar([]ent{file("single", "x", 0o4755)}))
	f.Add(buildTar([]ent{file("../x", "x", 0o644)}))
	f.Add(buildTar([]ent{file("/etc/x", "x", 0o644)}))
	f.Add(buildTar([]ent{dir("root"), special(rootA, tar.TypeSymlink, "/tmp"), file("root/a/f", "x", 0o644)}))
	f.Add(buildTar([]ent{dir("root"), special("root/h", tar.TypeLink, "../x")}))
	f.Add(buildTar([]ent{file("root/a/../../b", "x", 0o644)}))
	f.Add(buildTar([]ent{dir("root"), file("other/f", "x", 0o644)}))
	f.Add([]byte("not a tar"))
	f.Fuzz(func(t *testing.T, data []byte) {
		base := t.TempDir()
		parent := mustMkdir(t, filepath.Join(base, "parent"))
		dst := filepath.Join(parent, "dst")

		staged, err := transfer.Extract(bytes.NewReader(data), dst)
		if err != nil {
			requireClean(t, parent)
		} else {
			if left := tempLeftovers(t, parent); len(left) != 1 {
				t.Errorf("temp dirs after success = %v, want one", left)
			}

			abort(t, staged)
			requireClean(t, parent)
		}

		if entries, _ := os.ReadDir(base); len(entries) != 1 {
			t.Errorf("base has %d entries, want only parent", len(entries))
		}
	})
}
