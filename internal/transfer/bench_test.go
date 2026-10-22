package transfer_test

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

// BenchmarkExtractManySmallFiles measures per-entry overhead: 10,000 small
// files spread over 100 directories four levels deep, as in a source tree.
func BenchmarkExtractManySmallFiles(b *testing.B) {
	var archive bytes.Buffer

	writer := tar.NewWriter(&archive)
	body := []byte("package main\n")

	for i := range 10000 {
		dirIndex := i % 100
		hdr := new(tar.Header)
		hdr.Name = fmt.Sprintf("root/a%d/b%d/c%d/d%d/f%d.go", dirIndex%5, dirIndex%10, dirIndex%25, dirIndex, i)
		hdr.Typeflag = tar.TypeReg
		hdr.Mode = 0o644
		hdr.Size = int64(len(body))

		err := writer.WriteHeader(hdr)
		if err != nil {
			b.Fatal(err)
		}

		_, err = writer.Write(body)
		if err != nil {
			b.Fatal(err)
		}
	}

	err := writer.Close()
	if err != nil {
		b.Fatal(err)
	}

	parent := b.TempDir()
	b.ReportAllocs()
	b.SetBytes(int64(archive.Len()))
	b.ResetTimer()

	for i := range b.N {
		staged, err := transfer.Extract(bytes.NewReader(archive.Bytes()), filepath.Join(parent, fmt.Sprint("out", i)))
		if err != nil {
			b.Fatal(err)
		}

		err = staged.Abort()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteTarManySmallFiles measures per-file overhead of an upload:
// 2,000 small files spread over 100 directories.
func BenchmarkWriteTarManySmallFiles(b *testing.B) {
	src := filepath.Join(b.TempDir(), "root")
	body := []byte("package main\n")

	for idx := range 2000 {
		dir := filepath.Join(src, fmt.Sprintf("d%d", idx%100))

		err := os.MkdirAll(dir, 0o750)
		if err != nil {
			b.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", idx)), body, 0o600)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_, err := transfer.WriteTar(io.Discard, src, "root", false)
		if err != nil {
			b.Fatal(err)
		}
	}
}
