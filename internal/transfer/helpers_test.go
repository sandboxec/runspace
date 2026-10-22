package transfer_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

// ent describes one archive entry for buildTar.
type ent struct {
	name string
	typ  byte
	mode int64
	link string
	body string
}

func buildTar(entries []ent) []byte {
	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	for _, entry := range entries {
		hdr := new(tar.Header)
		hdr.Name = entry.name
		hdr.Typeflag = entry.typ
		hdr.Mode = entry.mode
		hdr.Linkname = entry.link

		if entry.typ == tar.TypeReg {
			hdr.Size = int64(len(entry.body))
		}

		err := writer.WriteHeader(hdr)
		if err != nil {
			panic(err)
		}

		if entry.typ == tar.TypeReg {
			_, err = io.WriteString(writer, entry.body)
			if err != nil {
				panic(err)
			}
		}
	}

	err := writer.Close()
	if err != nil {
		panic(err)
	}

	return buf.Bytes()
}

func file(name, body string, mode int64) ent {
	return ent{name: name, typ: tar.TypeReg, mode: mode, link: "", body: body}
}

func dir(name string) ent { return dirWithMode(name, 0o755) }

func dirWithMode(name string, mode int64) ent {
	return ent{name: name, typ: tar.TypeDir, mode: mode, link: "", body: ""}
}

// special makes an entry that has no content: a link, device or FIFO.
func special(name string, typ byte, link string) ent {
	return ent{name: name, typ: typ, mode: 0, link: link, body: ""}
}

// parentDir makes an empty directory to extract into.
func parentDir(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "parent")
}

// tempLeftovers lists .runspace-* entries in dir.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, ".runspace-*"))
	if err != nil {
		t.Fatal(err)
	}

	return matches
}

// requireClean fails unless parent is empty.
func requireClean(t *testing.T, parent string) {
	t.Helper()

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		t.Errorf("leftover in %s: %s", parent, entry.Name())
	}
}

func mustMkdir(t *testing.T, dirPath string) string {
	t.Helper()

	err := os.MkdirAll(dirPath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	return dirPath
}

func writeFile(t *testing.T, filePath, body string, mode fs.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(filePath))

	err := os.WriteFile(filePath, []byte(body), mode)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chmod(filePath, mode)
	if err != nil {
		t.Fatal(err)
	}
}

// readFile returns the content of the file at filePath.
func readFile(t *testing.T, filePath string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(filePath))
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// hashTree maps each regular file below root to its sha256.
func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()

	sums := map[string]string{}
	tree := os.DirFS(root)

	err := fs.WalkDir(tree, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return fmt.Errorf("hash %s: %w", name, err)
		}

		sum := sha256.Sum256(data)
		sums[name] = hex.EncodeToString(sum[:])

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return sums
}

// tarHeaders returns the headers of every entry in an archive.
func tarHeaders(t *testing.T, data []byte) []*tar.Header {
	t.Helper()

	var headers []*tar.Header

	reader := tar.NewReader(bytes.NewReader(data))

	for {
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return headers
		}

		if err != nil {
			t.Fatal(err)
		}

		headers = append(headers, hdr)
	}
}

// tarNames lists entry names and types in an archive.
func tarNames(t *testing.T, data []byte) map[string]byte {
	t.Helper()

	got := map[string]byte{}

	for _, hdr := range tarHeaders(t, data) {
		got[hdr.Name] = hdr.Typeflag
	}

	return got
}

func contains(t *testing.T, err error, subs ...string) {
	t.Helper()

	if err == nil {
		t.Fatalf("got nil error, want one containing %q", subs)
	}

	for _, sub := range subs {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err, sub)
		}
	}
}

// mustExtract extracts the archive built from entries into dst and fails the
// test on error.
func mustExtract(t *testing.T, entries []ent, dst string) *transfer.Staged {
	t.Helper()

	staged, err := transfer.Extract(bytes.NewReader(buildTar(entries)), dst)
	if err != nil {
		t.Fatal(err)
	}

	return staged
}

// mustCommit commits staged and fails the test on error.
func mustCommit(t *testing.T, staged *transfer.Staged) {
	t.Helper()

	err := staged.Commit()
	if err != nil {
		t.Fatal(err)
	}
}

// abort aborts staged and fails the test on error.
func abort(t *testing.T, staged *transfer.Staged) {
	t.Helper()

	err := staged.Abort()
	if err != nil {
		t.Error(err)
	}
}
