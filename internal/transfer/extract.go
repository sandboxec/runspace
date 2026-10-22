package transfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// resultName is where the archive root is staged inside the temp dir.
	resultName = "result"
	// tempPattern names the temp dir created next to the destination.
	tempPattern = ".runspace-*"
	// trailerSize is the length of a tar end-of-archive marker.
	trailerSize = 1024
	// dirMode is the permission of every extracted directory.
	dirMode fs.FileMode = 0o755
	// plainFileMode and execFileMode are the only permissions an extracted
	// file gets; the umask still applies.
	plainFileMode fs.FileMode = 0o644
	execFileMode  fs.FileMode = 0o755
	// anyExecBit is set in a tar mode when any of user, group or other may
	// execute the file.
	anyExecBit = 0o111
	// copyBufSize is the size of the buffer shared by all file copies.
	copyBufSize = 32 << 10
)

// Errors for link entries, which downloads refuse. Callers add the remedy
// in their own terms.
var (
	ErrSymlink = errors.New("downloads do not accept symbolic links")
	// ErrHardlink is separate because following symlinks (tar -h) still
	// records repeated hard links.
	ErrHardlink = errors.New("downloads do not accept hard links")
)

// Errors for refused extractions and commits. Each is wrapped with the entry
// or path it concerns, so the message reads as one sentence.
var (
	errCommitDone      = errors.New("already committed or aborted")
	errReplaceWithDir  = errors.New("exists and is not a directory; a downloaded directory cannot replace it")
	errDstIsDir        = errors.New("already exists and is a directory; downloads never merge into an existing directory")
	errNoEntries       = errors.New("extract: archive has no entries")
	errOutsideRoot     = errors.New("outside the archive root")
	errUnsupportedType = errors.New("only regular files and directories can be downloaded")

	errEmptyName    = errors.New("empty name")
	errNULName      = errors.New("name contains NUL")
	errAbsoluteName = errors.New("absolute path")
	errDotDotName   = errors.New(`path has a ".." element`)
	errNotLocalName = errors.New("not a local path")
)

// Staged is a downloaded tree held in a temporary directory next to its
// destination. Nothing exists at the destination until [Staged.Commit].
type Staged struct {
	dst   string
	tmp   string // temporary directory; empty once committed or aborted
	stats Stats
}

// Extract reads a tar stream whose entries share one top-level component and
// stages its contents in a new temporary directory next to dst. Nothing
// appears at dst until Commit. The parent of dst must exist.
//
// The stream is untrusted. Extract writes only through an [os.Root] on the
// temporary directory, never creates links, and accepts only regular files and
// directories; any other entry fails the whole extraction. After a successful
// extraction it reads the stream to its end. On error it removes the
// temporary directory before returning.
func Extract(archive io.Reader, dst string) (*Staged, error) {
	dst = filepath.Clean(dst)

	_, err := checkDst(dst)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dst), tempPattern)
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", dst, err)
	}

	stats, err := extractInto(tmp, archive)
	if err != nil {
		// The extract's own error matters more than a failure to clean up.
		_ = os.RemoveAll(tmp)

		return nil, err
	}

	return &Staged{dst: dst, tmp: tmp, stats: stats}, nil
}

// Stats reports what the staged tree contains.
func (s *Staged) Stats() Stats { return s.stats }

// Commit renames the staged result to the destination and removes the
// temporary directory. An existing directory destination is refused, as is
// replacing an existing non-directory with a directory. On any error nothing
// is changed at the destination and the temporary directory is removed.
func (s *Staged) Commit() error {
	if s.tmp == "" {
		return fmt.Errorf("commit %s: %w", s.dst, errCommitDone)
	}

	err := s.rename()
	if err != nil {
		// The rename's own error matters more than a failure to clean up.
		_ = s.Abort()

		return err
	}

	err = s.Abort()
	if err != nil {
		return fmt.Errorf("commit %s: committed, but removing temporary directory failed: %w", s.dst, err)
	}

	return nil
}

// Abort removes the temporary directory. It does nothing after Commit or a
// previous Abort.
func (s *Staged) Abort() error {
	tmp := s.tmp

	s.tmp = ""

	if tmp == "" {
		return nil
	}

	err := os.RemoveAll(tmp)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func (s *Staged) rename() error {
	staged := filepath.Join(s.tmp, resultName)

	result, err := os.Lstat(staged)
	if err != nil {
		return fmt.Errorf("commit %s: %w", s.dst, err)
	}

	dstExists, err := checkDst(s.dst)
	if err != nil {
		return err
	}

	if dstExists && result.IsDir() {
		return fmt.Errorf("commit %s: %w", s.dst, errReplaceWithDir)
	}

	err = os.Rename(staged, s.dst)
	if err != nil {
		return fmt.Errorf("commit %s: %w", s.dst, err)
	}

	return nil
}

// checkDst reports whether dst exists. It fails if dst is a directory:
// downloads never merge into one.
func checkDst(dst string) (bool, error) {
	info, err := os.Lstat(dst)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("extract %s: %w", dst, err)
	case info.IsDir():
		return false, fmt.Errorf("%s %w", dst, errDstIsDir)
	}

	return true, nil
}

// extractInto extracts archive into the existing directory tmp through an
// os.Root.
func extractInto(tmp string, archive io.Reader) (Stats, error) {
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return Stats{}, fmt.Errorf("extract: %w", err)
	}

	tail := &zeroTail{r: archive, zeros: 0}
	ext := &extractor{root: root, base: "", stats: Stats{Files: 0, Bytes: 0}, buf: nil}

	err = ext.run(tar.NewReader(tail), tail)

	closeErr := root.Close()
	if err == nil && closeErr != nil {
		err = fmt.Errorf("extract: %w", closeErr)
	}

	if err != nil {
		return Stats{}, err
	}

	return ext.stats, nil
}

type extractor struct {
	root  *os.Root
	base  string // archive root component, set by the first entry
	stats Stats
	// buf is reused for every file. Copying into an *os.File directly
	// would take its ReadFrom path, which allocates a buffer per file.
	buf []byte
}

// writerOnly hides a writer's ReadFrom, so io.CopyBuffer uses the buffer.
type writerOnly struct{ io.Writer }

func (x *extractor) run(entries *tar.Reader, tail *zeroTail) error {
	for {
		// Count only what Next consumes (padding, headers, the trailer), not
		// file content: a file may legitimately end in zeros.
		tail.zeros = 0

		hdr, err := entries.Next()
		// Next returns io.EOF itself at the end of the archive. A stream
		// error that wraps io.EOF, such as a lost connection, is a failure.
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("extract: read archive: %w", err)
		}

		entryErr := x.entry(hdr, entries)
		if entryErr != nil {
			return entryErr
		}
	}

	if x.base == "" {
		return errNoEntries
	}
	// tar.Reader reports a stream cut between entries as a clean end.
	if tail.zeros < trailerSize {
		return fmt.Errorf("extract: read archive: missing end-of-archive marker: %w", io.ErrUnexpectedEOF)
	}
	// Let the writer finish: the marker is usually followed by block padding.
	_, err := io.Copy(io.Discard, tail)
	if err != nil {
		return fmt.Errorf("extract: read archive: %w", err)
	}

	return nil
}

func (x *extractor) entry(hdr *tar.Header, content io.Reader) error {
	rel, err := x.place(hdr.Name)
	if err != nil {
		return err
	}

	target := resultName
	if rel != "" {
		target = filepath.Join(resultName, filepath.FromSlash(rel))
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return x.mkdir(hdr.Name, target)
	case tar.TypeReg:
		return x.file(hdr, target, content)
	}

	return unsupported(hdr)
}

// place validates an entry name and returns its path below the archive root,
// or "" for the root itself. The first entry fixes the archive root.
func (x *extractor) place(name string) (string, error) {
	err := checkName(name)
	if err != nil {
		return "", fmt.Errorf("extract: unsafe entry %q: %w", name, err)
	}

	top, rel, _ := strings.Cut(path.Clean(name), "/")
	if x.base == "" {
		x.base = top
	} else if top != x.base {
		return "", fmt.Errorf("extract: entry %q is %w %q", name, errOutsideRoot, x.base)
	}

	return rel, nil
}

// checkName rejects entry names that are empty, absolute, contain NUL or a
// ".." component, or are not local paths on this platform.
func checkName(name string) error {
	switch {
	case name == "":
		return errEmptyName
	case strings.ContainsRune(name, 0):
		return errNULName
	case strings.HasPrefix(name, "/"):
		return errAbsoluteName
	case slices.Contains(strings.Split(name, "/"), ".."):
		return errDotDotName
	}

	clean := path.Clean(name)
	if clean == "." || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return errNotLocalName
	}

	return nil
}

func (x *extractor) mkdir(name, target string) error {
	err := x.root.MkdirAll(target, dirMode)
	if err != nil {
		return fmt.Errorf("extract %q: %w", name, err)
	}

	return nil
}

// file creates a regular file, and any missing parent directories, from
// content.
func (x *extractor) file(hdr *tar.Header, target string, content io.Reader) error {
	dir := filepath.Dir(target)
	if dir != "." {
		err := x.mkdir(hdr.Name, dir)
		if err != nil {
			return err
		}
	}

	written, err := x.create(hdr, target, content)
	if err != nil {
		return err
	}

	x.stats.Files++
	x.stats.Bytes += written

	return nil
}

// create writes content to a new file at target and returns the number of
// bytes written. The file gets mode 0644, or 0755 if the header has any
// execute bit. Setuid, setgid and sticky are never set; the umask applies.
func (x *extractor) create(hdr *tar.Header, target string, content io.Reader) (int64, error) {
	perm := plainFileMode
	if hdr.Mode&anyExecBit != 0 {
		perm = execFileMode
	}

	file, err := x.root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, fmt.Errorf("extract %q: %w", hdr.Name, err)
	}

	if x.buf == nil {
		x.buf = make([]byte, copyBufSize)
	}

	written, err := io.CopyBuffer(writerOnly{Writer: file}, content, x.buf)
	closeErr := file.Close()

	if err != nil {
		return 0, fmt.Errorf("extract %q: %w", hdr.Name, err)
	}

	if closeErr != nil {
		return 0, fmt.Errorf("extract %q: %w", hdr.Name, closeErr)
	}

	return written, nil
}

// entryKind names a tar entry type for error messages.
func entryKind(typeflag byte) string {
	kinds := map[byte]string{
		tar.TypeSymlink:       "symlink",
		tar.TypeLink:          "hardlink",
		tar.TypeChar:          "character device",
		tar.TypeBlock:         "block device",
		tar.TypeFifo:          "FIFO",
		tar.TypeCont:          "contiguous file",
		tar.TypeGNUSparse:     "sparse file",
		tar.TypeXGlobalHeader: "pax global header",
	}

	kind, known := kinds[typeflag]
	if !known {
		return fmt.Sprintf("entry of type %q", typeflag)
	}

	return kind
}

// unsupported builds the error for an entry that is neither a file nor a
// directory.
func unsupported(hdr *tar.Header) error {
	kind := entryKind(hdr.Typeflag)

	switch hdr.Typeflag {
	case tar.TypeSymlink:
		return fmt.Errorf("extract: entry %q is a %s: %w", hdr.Name, kind, ErrSymlink)
	case tar.TypeLink:
		return fmt.Errorf("extract: entry %q is a %s: %w", hdr.Name, kind, ErrHardlink)
	}

	return fmt.Errorf("extract: entry %q is a %s; %w", hdr.Name, kind, errUnsupportedType)
}

// zeroTail passes reads through and tracks how many zero bytes end the stream
// so far.
type zeroTail struct {
	r     io.Reader
	zeros int
}

// Read returns the wrapped reader's result unchanged: callers such as
// tar.Reader compare the error with io.EOF, which wrapping would hide.
func (z *zeroTail) Read(buf []byte) (int, error) {
	count, err := z.r.Read(buf)

	end := count
	for end > 0 && buf[end-1] == 0 {
		end--
	}

	if end == 0 {
		z.zeros += count
	} else {
		z.zeros = count - end
	}

	return count, err //nolint:wrapcheck // pass-through reader: wrapping would hide io.EOF from == comparisons
}
