package transfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Errors for uploads that cannot proceed. Each is wrapped with the name or
// path it concerns.
var (
	errInvalidName   = errors.New("invalid entry name")
	errNotOneElement = errors.New("must be a single path element")
	errCannotArchive = errors.New("cannot archive")
	errDirectoryLoop = errors.New("directory loop: already being archived as an ancestor")
	errFileShrank    = errors.New("file shrank while being read")
)

// WriteTar writes src (a local file or directory) to w as a tar stream whose
// top-level entry is named name (the basename of the remote DST); the caller
// extracts it remotely into dirname(DST). If follow is true, WriteTar follows
// symlinks in src, including src itself; otherwise it stores them as symlinks.
//
// On error the stream is left without an end-of-archive marker, so a reader
// sees it as truncated.
func WriteTar(out io.Writer, src, name string, follow bool) (Stats, error) {
	err := checkTopName(name)
	if err != nil {
		return Stats{}, err
	}

	writer := &uploader{tw: tar.NewWriter(out), follow: follow, stats: Stats{Files: 0, Bytes: 0}, active: nil, buf: nil}

	err = writer.add(src, name)
	if err != nil {
		return Stats{}, err
	}

	err = writer.tw.Close()
	if err != nil {
		return Stats{}, fmt.Errorf("tar %s: close archive: %w", src, err)
	}

	return writer.stats, nil
}

// checkTopName reports whether name is a single, usable path element.
func checkTopName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("tar: %w %q", errInvalidName, name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("tar: entry name %q %w", name, errNotOneElement)
	}

	return nil
}

type uploader struct {
	tw     *tar.Writer
	follow bool
	stats  Stats
	active []fs.FileInfo // directories being walked, outermost first
	// buf is reused for every file. Copying from an *os.File directly would
	// take its WriteTo path, which allocates a buffer per file.
	buf []byte
}

// readerOnly hides a reader's WriteTo, so io.CopyBuffer uses the buffer.
type readerOnly struct{ io.Reader }

// add writes the entry for the file at srcPath, and everything below it, as
// name.
func (u *uploader) add(srcPath, name string) error {
	stat := os.Lstat
	if u.follow {
		stat = os.Stat
	}

	info, err := stat(srcPath)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	switch mode := info.Mode(); {
	case mode.IsDir():
		return u.addDir(srcPath, name, info)
	case mode.IsRegular():
		return u.addFile(srcPath, name, info)
	case mode&fs.ModeSymlink != 0:
		return u.addSymlink(srcPath, name, info)
	default:
		return fmt.Errorf("tar %s: %w %s", srcPath, errCannotArchive, fileKind(mode))
	}
}

func (u *uploader) addDir(srcPath, name string, info fs.FileInfo) error {
	for _, parent := range u.active {
		if os.SameFile(parent, info) {
			return fmt.Errorf("tar %s: %w", srcPath, errDirectoryLoop)
		}
	}

	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	err = u.header(srcPath, name+"/", "", info)
	if err != nil {
		return err
	}

	u.active = append(u.active, info)
	defer func() { u.active = u.active[:len(u.active)-1] }()

	for _, entry := range entries {
		child := filepath.Join(srcPath, entry.Name())

		err = u.add(child, name+"/"+filepath.ToSlash(entry.Name()))
		if err != nil {
			return err
		}
	}

	return nil
}

func (u *uploader) addSymlink(srcPath, name string, info fs.FileInfo) error {
	target, err := os.Readlink(srcPath)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	return u.header(srcPath, name, target, info)
}

func (u *uploader) addFile(srcPath, name string, info fs.FileInfo) error {
	file, err := os.Open(srcPath) //nolint:gosec // G304: the caller chooses which of its own files to upload
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	err = u.copyFile(file, srcPath, name, info)
	closeErr := file.Close()

	if err != nil {
		return err
	}

	if closeErr != nil {
		return fmt.Errorf("tar %s: %w", srcPath, closeErr)
	}

	return nil
}

// copyFile writes the header for file and then its content. It fails if the
// file holds fewer bytes than the header promised, so the error names the
// file instead of surfacing at the next entry.
func (u *uploader) copyFile(file io.Reader, srcPath, name string, info fs.FileInfo) error {
	err := u.header(srcPath, name, "", info)
	if err != nil {
		return err
	}

	if u.buf == nil {
		u.buf = make([]byte, copyBufSize)
	}

	written, err := io.CopyBuffer(u.tw, readerOnly{Reader: file}, u.buf)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	if written != info.Size() {
		return fmt.Errorf("tar %s: %w", srcPath, errFileShrank)
	}

	u.stats.Files++
	u.stats.Bytes += written

	return nil
}

// header writes the header for info under name. Ownership is dropped; mode and
// modification time come from the source.
func (u *uploader) header(srcPath, name, link string, info fs.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	hdr.Name = name
	hdr.Uid, hdr.Gid = 0, 0
	hdr.Uname, hdr.Gname = "", ""

	err = u.tw.WriteHeader(hdr)
	if err != nil {
		return fmt.Errorf("tar %s: %w", srcPath, err)
	}

	return nil
}

// fileKind describes a mode that tar cannot carry.
func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeNamedPipe != 0:
		return "a FIFO"
	case mode&fs.ModeCharDevice != 0:
		return "a character device"
	case mode&fs.ModeDevice != 0:
		return "a block device"
	}

	return "a " + mode.Type().String() + " file"
}
