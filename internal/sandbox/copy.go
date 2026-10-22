//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/transfer"
)

var (
	errNotOneRemote = errors.New("must be a sandbox path, written NAME:PATH")
	errEmptyPath    = errors.New("src and dst must not be empty")
	errRemoteTar    = errors.New("remote tar exited")

	// errUploadStopped ends an upload whose remote side stopped reading.
	errUploadStopped = errors.New("upload stopped")
)

// location is one side of cp.
type location struct {
	// name is the sandbox name, or "" for the host.
	name string
	// path is the host or sandbox path, or "-" for a host tar stream.
	path string
}

func (l location) remote() bool { return l.name != "" }

// parseLocation splits NAME:PATH. Sandbox names never contain ':', so the
// first ':' splits unambiguously; a host path containing ':' needs a ./
// prefix. A Windows drive letter is always a host path.
func parseLocation(text string) location {
	if text == "-" || filepath.VolumeName(text) != "" {
		return location{name: "", path: text}
	}

	if name, path, ok := strings.Cut(text, ":"); ok && github.ValidCodespaceName(name) {
		return location{name: name, path: path}
	}

	return location{name: "", path: text}
}

// locations parses and checks both sides of cp: exactly one is in a
// sandbox.
func (o *CopyOptions) locations() (location, location, error) {
	src, dst := parseLocation(o.Src), parseLocation(o.Dst)

	switch {
	case src.remote() == dst.remote():
		return src, dst, fmt.Errorf("exactly one of src %q and dst %q %w", o.Src, o.Dst, errNotOneRemote)
	case src.path == "" && !src.remote(), dst.path == "" && !dst.remote():
		return src, dst, errEmptyPath
	}

	return src, dst, nil
}

// CopyResult describes a finished cp. Files and Bytes count regular files;
// they are zero for "-" streams, which runspace does not inspect.
type CopyResult struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// extractScript extracts a tar stream from stdin into $1, creating $1 first.
const extractScript = `mkdir -p -- "$1" && cd -- "$1" && tar -xf -`

// Copy copies between the host and a sandbox, streaming tar through one SSH
// session. Downloads are contained by package transfer.
func (s *Sandboxes) Copy(ctx context.Context, opts CopyOptions, streams Streams) (CopyResult, error) {
	err := opts.Validate()
	if err != nil {
		return CopyResult{}, err
	}

	stats, err := s.copyBetween(ctx, &opts, streams)

	err = linkHint(opts.explain("the copy", err))
	if err != nil {
		return CopyResult{}, fmt.Errorf("cp %s %s: %w", opts.Src, opts.Dst, err)
	}

	return CopyResult{Src: opts.Src, Dst: opts.Dst, Files: stats.Files, Bytes: stats.Bytes}, nil
}

// linkHint says what to do about a refused symbolic or hard link.
func linkHint(err error) error {
	switch {
	case errors.Is(err, transfer.ErrSymlink):
		return fmt.Errorf("%w; set -L (follow_link) to copy what links point to", err)
	case errors.Is(err, transfer.ErrHardlink):
		return fmt.Errorf("%w; copy each linked file by its own path, "+
			"or, in the CLI, stream the raw archive with cp NAME:PATH -", err)
	}

	return err
}

// copyBetween runs a validated cp.
func (s *Sandboxes) copyBetween(ctx context.Context, opts *CopyOptions, streams Streams) (transfer.Stats, error) {
	src, dst := opts.srcLoc, opts.dstLoc

	side := src
	if dst.remote() {
		side = dst
	}
	// Fail fast, before touching the sandbox, when the host source is missing.
	if dst.remote() && src.path != "-" {
		_, err := os.Lstat(src.path)
		if err != nil {
			return transfer.Stats{}, fmt.Errorf("%w", err)
		}
	}

	held, err := s.acquire(ctx, side.name, streams.progress())
	if err != nil {
		return transfer.Stats{}, err
	}

	remotePath := workdir(held.workspace, side.path)
	remoteTar := &copier{
		conn:         held.conn,
		workspace:    held.workspace,
		follow:       opts.FollowLink,
		stderr:       new(capped),
		transportErr: nil,
	}
	// The timeout bounds the transfer, not the dial.
	ctx, cancel := opts.deadline(ctx)
	defer cancel()

	stats, err := remoteTar.move(ctx, src, dst, remotePath, streams)
	held.release(remoteTar.transportErr)

	return stats, err
}

// copier runs the remote half of one cp.
type copier struct {
	conn      Conn
	workspace string
	follow    bool
	stderr    *capped
	// transportErr is set when the remote command broke the Conn.
	transportErr error
}

// extract is the remote command that extracts stdin into dir.
func extract(dir string) []string {
	return scriptArgv(extractScript, dir)
}

// archive is the remote command that writes remotePath as a tar stream whose
// single top-level entry is its base name. -h follows symlinks under -L.
func (c *copier) archive(remotePath string) []string {
	flags := "-cf"
	if c.follow {
		flags = "-chf"
	}

	script := `cd -- "$1" && tar ` + flags + ` - -- "$2"`

	return scriptArgv(script, path.Dir(remotePath), path.Base(remotePath))
}

// move moves data between src and dst, one of which is in the sandbox
// at remotePath.
func (c *copier) move(
	ctx context.Context, src, dst location, remotePath string, streams Streams,
) (transfer.Stats, error) {
	switch {
	case dst.remote() && src.path == "-":
		return transfer.Stats{}, c.run(ctx, extract(remotePath), streams.Stdin, nil)
	case dst.remote():
		return c.upload(ctx, src.path, remotePath)
	case dst.path == "-":
		return transfer.Stats{}, c.run(ctx, c.archive(remotePath), nil, streams.Stdout)
	default:
		return c.download(ctx, remotePath, dst.path)
	}
}

// run runs argv in the workspace.
func (c *copier) run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	code, err := c.conn.Exec(ctx, remote.Cmd{
		Dir:    c.workspace,
		Env:    nil,
		Argv:   argv,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: c.stderr,
		TTY:    nil,
	})
	// ctx is this command's own: a download cancels it when extraction
	// fails, which is not the transport's fault.
	c.transportErr = transportFailure(ctx, err)

	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case code != 0:
		return exited(errRemoteTar, code, c.stderr)
	}

	return nil
}

// upload streams the host path src into the sandbox as dst: the entries are
// named base(dst) and extracted into dir(dst), which is created if missing.
func (c *copier) upload(ctx context.Context, src, dst string) (transfer.Stats, error) {
	pipeReader, pipeWriter := io.Pipe()
	written := make(chan error, 1)

	var stats transfer.Stats

	go func() {
		var err error

		stats, err = transfer.WriteTar(pipeWriter, src, path.Base(dst), c.follow)
		_ = pipeWriter.CloseWithError(err) // Always nil.

		written <- err
	}()

	err := c.run(ctx, extract(path.Dir(dst)), pipeReader, nil)
	// Unblock the writer if the remote side stopped reading early.
	_ = pipeReader.CloseWithError(errUploadStopped) // Always nil.
	// A local failure, such as an unreadable file, explains whatever the
	// remote tar reported about the cut-off stream.
	writeErr := <-written
	if writeErr != nil && !errors.Is(writeErr, errUploadStopped) {
		return transfer.Stats{}, writeErr
	}

	return stats, err
}

// download streams the sandbox path src into the host path dst. The
// result appears at dst only after the stream ends cleanly and the remote
// tar exits 0; on any failure nothing appears.
func (c *copier) download(ctx context.Context, src, dst string) (transfer.Stats, error) {
	// An extraction error, such as a refused entry, cancels the remote tar,
	// which would otherwise block once nobody reads its output.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	pipeReader, pipeWriter := io.Pipe()

	type extracted struct {
		staged *transfer.Staged
		err    error
	}

	done := make(chan extracted, 1)

	go func() {
		staged, err := transfer.Extract(pipeReader, dst)
		if err != nil {
			cancel(err)
			_ = pipeReader.CloseWithError(err) // Always nil.
		}

		done <- extracted{staged, err}
	}()

	err := c.run(ctx, c.archive(src), nil, pipeWriter)
	_ = pipeWriter.CloseWithError(err) // Always nil.

	result := <-done
	if result.err != nil {
		// The extraction error is the real cause of a cancelled tar.
		return transfer.Stats{}, result.err
	}

	if err != nil {
		// Best effort: the download already failed.
		_ = result.staged.Abort()

		return transfer.Stats{}, err
	}

	err = result.staged.Commit()
	if err != nil {
		return transfer.Stats{}, fmt.Errorf("%w", err)
	}

	return result.staged.Stats(), nil
}
