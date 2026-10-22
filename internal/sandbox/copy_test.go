//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// fakeTar plays the remote tar: archive commands write stream and exit with
// code; extract commands read stdin into got.
type fakeTar struct {
	stream []byte
	code   int
	got    bytes.Buffer
	argv   [][]string
}

func newFakeTar(stream []byte, code int) *fakeTar {
	return &fakeTar{stream: stream, code: code, got: bytes.Buffer{}, argv: nil}
}

func (f *fakeTar) command(_ context.Context, cmd remote.Cmd) (int, error) {
	if len(cmd.Argv) < 3 {
		return 0, nil
	}

	f.argv = append(f.argv, cmd.Argv)

	switch script := cmd.Argv[2]; {
	case strings.Contains(script, "tar -x"):
		_, err := io.Copy(&f.got, cmd.Stdin)
		if err != nil {
			return f.code, fmt.Errorf("read the archive: %w", err)
		}

		return f.code, nil
	case strings.Contains(script, "tar -c"):
		_, err := cmd.Stdout.Write(f.stream)
		if err != nil {
			return f.code, fmt.Errorf("write the archive: %w", err)
		}

		return f.code, nil
	}

	return 0, nil
}

func dirEntry(name string) *tar.Header {
	hdr := new(tar.Header)
	hdr.Name = name
	hdr.Typeflag = tar.TypeDir
	hdr.Mode = 0o755

	return hdr
}

func fileEntry(name string, size int64) *tar.Header {
	hdr := new(tar.Header)
	hdr.Name = name
	hdr.Typeflag = tar.TypeReg
	hdr.Mode = 0o644
	hdr.Size = size

	return hdr
}

func symlinkEntry(name, target string) *tar.Header {
	hdr := new(tar.Header)
	hdr.Name = name
	hdr.Typeflag = tar.TypeSymlink
	hdr.Linkname = target

	return hdr
}

func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	for _, hdr := range entries {
		err := writer.WriteHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}

		if hdr.Typeflag == tar.TypeReg {
			_, err = writer.Write([]byte(strings.Repeat("x", int(hdr.Size))))
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func newCopyFixture(t *testing.T, remoteTar *fakeTar) (*fixture, string) {
	t.Helper()

	rig := newFixture(t, true)
	rig.dial.Handler = userCmd(remoteTar.command)

	return rig, rig.mustCreate(context.Background(), t)
}

func TestCopyDownload(t *testing.T) {
	t.Parallel()

	remoteTar := newFakeTar(tarOf(t, dirEntry("tree/"), fileEntry("tree/a.txt", 3)), 0)
	rig, name := newCopyFixture(t, remoteTar)
	dst := filepath.Join(t.TempDir(), "out")

	res, err := rig.Copy(context.Background(), newCopy(name+":tree", dst), noStreams())
	if err != nil {
		t.Fatal(err)
	}

	content, _ := os.ReadFile(filepath.Clean(filepath.Join(dst, "a.txt")))
	if string(content) != "xxx" || res.Files != 1 || res.Bytes != 3 {
		t.Errorf("result %+v, content %q", res, content)
	}

	if got := remoteTar.argv[0]; !slices.Contains(got, testWorkspace) || !slices.Contains(got, "tree") {
		t.Errorf("archive argv = %q", got)
	}
}

// A refused entry or a failed remote tar leaves no DST, and neither discards
// the cached Conn.
func TestCopyDownloadFailures(t *testing.T) {
	t.Parallel()

	for name, remoteTar := range map[string]*fakeTar{
		"symlink": newFakeTar(tarOf(t, symlinkEntry("tree", "/etc")), 0),
		"remote":  newFakeTar(tarOf(t, fileEntry("tree", 1)), 2),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rig, sandboxName := newCopyFixture(t, remoteTar)
			dir := t.TempDir()

			_, err := rig.Copy(context.Background(), newCopy(sandboxName+":tree", filepath.Join(dir, "out")), noStreams())
			if err == nil {
				t.Fatal("copy succeeded")
			}

			if name == "symlink" && !strings.Contains(err.Error(), "-L") {
				t.Errorf("%v", err)
			}

			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("left %v", entries)
			}

			if n := rig.dial.Dials(sandboxName); n != 1 {
				t.Errorf("dials = %d; the Conn was discarded", n)
			}
		})
	}
}

func TestCopyUpload(t *testing.T) {
	t.Parallel()

	remoteTar := newFakeTar(nil, 0)
	rig, name := newCopyFixture(t, remoteTar)
	src := filepath.Join(t.TempDir(), "in.txt")
	writeFile(t, src, "hello", 0o644)

	res, err := rig.Copy(context.Background(), newCopy(src, name+":dir/copy.txt"), noStreams())
	if err != nil || res.Files != 1 || res.Bytes != 5 {
		t.Fatalf("%+v, %v", res, err)
	}

	hdr, err := tar.NewReader(&remoteTar.got).Next()
	if err != nil || hdr.Name != "copy.txt" {
		t.Errorf("uploaded entry %v, %v", hdr, err)
	}

	if got := remoteTar.argv[0]; got[len(got)-1] != testWorkspace+"/dir" {
		t.Errorf("extract argv = %q", got)
	}
}

// A local upload failure is reported as itself and keeps the Conn.
func TestCopyUploadLocalFailures(t *testing.T) {
	t.Parallel()

	remoteTar := newFakeTar(nil, 0)
	rig, name := newCopyFixture(t, remoteTar)
	missing := filepath.Join(t.TempDir(), "typo")

	_, err := rig.Copy(context.Background(), newCopy(missing, name+":x"), noStreams())
	if !errors.Is(err, os.ErrNotExist) || len(remoteTar.argv) != 0 {
		t.Errorf("missing source: %v; remote ran %d commands", err, len(remoteTar.argv))
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.txt"), "ok", 0o644)
	writeFile(t, filepath.Join(dir, "secret.txt"), "no", 0o000)

	_, err = rig.Copy(context.Background(), newCopy(dir, name+":x"), noStreams())
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("unreadable file: %v", err)
	}

	if n := rig.dial.Dials(name); n != 1 {
		t.Errorf("dials = %d; a local failure discarded the Conn", n)
	}
}

func TestCopyStreams(t *testing.T) {
	t.Parallel()

	stream := tarOf(t, fileEntry("-", 2))
	remoteTar := newFakeTar(stream, 0)
	rig, name := newCopyFixture(t, remoteTar)
	ctx := context.Background()

	var out bytes.Buffer

	_, err := rig.Copy(ctx, newCopy(name+":tree", "-"), streamsTo(&out))
	if err != nil || !bytes.Equal(out.Bytes(), stream) {
		t.Errorf("NAME:tree -: %v, %d bytes", err, out.Len())
	}

	_, err = rig.Copy(ctx, newCopy("-", name+":dir"), streamsFrom(strings.NewReader("raw")))
	if err != nil || remoteTar.got.String() != "raw" {
		t.Errorf("- NAME:dir: %v, %q", err, remoteTar.got.String())
	}
	// NAME:- is a sandbox file named "-", not a stream.
	dst := filepath.Join(t.TempDir(), "dash")

	_, err = rig.Copy(ctx, newCopy(name+":-", dst), noStreams())
	if err != nil {
		t.Fatal(err)
	}

	if last := remoteTar.argv[len(remoteTar.argv)-1]; last[len(last)-1] != "-" || !strings.Contains(last[2], "tar -c") {
		t.Errorf("NAME:- ran %q", last)
	}
}

// A cancel that arrives during the POST still deletes the codespace.
func TestCreateCancelledDuringPOST(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	rig.gh.OnCreate = cancel

	_, err := rig.Create(ctx, newCreateOptions(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	if !rig.gh.Called(deleteCS1) {
		t.Error("a create cancelled during the POST left its codespace")
	}
}

// A failed dial is not cached, even when every caller gave up.
func TestFailedAndAbandonedDial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, true)
	name := rig.mustCreate(ctx, t)
	rig.Forget(name)

	exec := func(ctx context.Context) error {
		return rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	}

	release := make(chan error)
	rig.dial.Hook = func(ctx context.Context, _ string) error {
		select {
		case err := <-release:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	waitCtx, cancel := context.WithCancel(ctx)

	done := make(chan error)

	go func() { done <- exec(waitCtx) }()

	waitFor(t, "the dial to start", func() bool { return rig.Cached(name) })
	cancel()

	err := <-done
	if sandbox.ExitCode(err) != sandbox.ExitInterrupted {
		t.Fatalf("abandoned call: %v", err)
	}

	release <- errTunnelRefused

	waitFor(t, "the failed dial to leave the cache", func() bool { return !rig.Cached(name) })

	rig.dial.Hook = nil

	err = exec(ctx)
	if err != nil {
		t.Errorf("after a failed, abandoned dial: %v", err)
	}
}

// Delete cancels a dial in progress.
func TestDeleteCancelsDial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, true)
	name := rig.mustCreate(ctx, t)
	rig.Forget(name)

	rig.dial.Hook = func(ctx context.Context, _ string) error {
		<-ctx.Done()

		return ctx.Err()
	}

	execDone := make(chan error)

	go func() {
		execDone <- rig.Exec(ctx, newExec(name, newCommand("x")), noStreams())
	}()

	waitFor(t, "the dial to start", func() bool { return rig.Cached(name) })

	_, err := rig.Delete(ctx, newDelete(name))
	if err != nil {
		t.Fatal(err)
	}

	err = <-execDone
	if sandbox.ExitCode(err) != sandbox.ExitFailure {
		t.Errorf("exec whose dial delete cancelled: %v", err)
	}
}

// stallAfterPrepare answers prepare and then never answers, so cp and diff
// run out of time.
func stallAfterPrepare(ctx context.Context, name string, cmd remote.Cmd) (int, error) {
	if sandboxtest.Step(cmd) == stepPrepare {
		return runspaceStep(ctx, name, cmd)
	}

	return blockUntilDone(ctx, cmd)
}

// timeoutCase is one of the cp and diff variants that run out of time.
// scratch is a directory the variant may fill with its own inputs, and
// outDir is where its result would appear.
type timeoutCase struct {
	name string
	run  func(rig *fixture, name, scratch, outDir string) error
}

func timeoutCases() []timeoutCase {
	timeout := newTimeout("1m")

	return []timeoutCase{
		{"download", func(rig *fixture, name, _, outDir string) error {
			opts := newCopy(name+":tree", filepath.Join(outDir, "out"))
			opts.TimeoutOption = timeout

			_, err := rig.Copy(context.Background(), opts, noStreams())
			if err != nil {
				return fmt.Errorf("download: %w", err)
			}

			return nil
		}},
		{"upload", func(rig *fixture, name, scratch, _ string) error {
			src := filepath.Join(scratch, "in.txt")

			err := os.WriteFile(src, []byte("data"), 0o600)
			if err != nil {
				return fmt.Errorf("write the source: %w", err)
			}

			opts := newCopy(src, name+":x")
			opts.TimeoutOption = timeout

			_, err = rig.Copy(context.Background(), opts, noStreams())
			if err != nil {
				return fmt.Errorf("upload: %w", err)
			}

			return nil
		}},
		{"diff -o", func(rig *fixture, name, _, outDir string) error {
			opts := newDiff(name, filepath.Join(outDir, "p.patch"))
			opts.TimeoutOption = timeout

			_, err := rig.Diff(context.Background(), opts, noStreams())
			if err != nil {
				return fmt.Errorf("diff: %w", err)
			}

			return nil
		}},
	}
}

// A transfer that runs out of time leaves no partial result and keeps the
// Conn, because the timeout is not the transport's fault.
func TestCopyAndDiffTimeout(t *testing.T) {
	t.Parallel()

	for _, test := range timeoutCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rig := newFixture(t, true)
				rig.dial.Handler = stallAfterPrepare
				name := rig.mustCreate(context.Background(), t)
				dir := t.TempDir()

				err := test.run(rig, name, t.TempDir(), dir)
				if err == nil || !strings.Contains(err.Error(), "did not finish within the 1m timeout") {
					t.Fatalf("err = %v", err)
				}

				entries, _ := os.ReadDir(dir)
				if len(entries) != 0 {
					t.Errorf("left %v", entries)
				}

				if n := rig.dial.Dials(name); n != 1 {
					t.Errorf("dials = %d; a timeout discarded the Conn", n)
				}
			})
		})
	}
}
