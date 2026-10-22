//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// The ways originRepo fails. originRepo adds "; pass -R OWNER/REPO" to each.
// None may carry the remote's URL, which can hold credentials.
var (
	errNotInRepo    = errors.New("not in a git repository")
	errMalformedGit = errors.New("malformed .git")
	errNoOrigin     = errors.New("git remote origin is not set")
	errUnreadable   = errors.New("git remote origin's url cannot be read")
	errNotOwned     = errors.New("is owned by another user")
	errNotRegular   = errors.New("is not a regular file")
	errTooLarge     = errors.New("is larger than 1 MiB")
	errNotGitHub    = errors.New("git remote origin is not on github.com")
	errBadRemote    = errors.New("git remote origin on github.com is not OWNER/REPO")
)

const (
	flagHint = "pass -R OWNER/REPO"

	// gitFile is the name of a repository's git directory, or of the file
	// that points to it.
	gitFile = ".git"

	// gitdirPrefix starts the one line of a .git file.
	gitdirPrefix = "gitdir:"

	// byteOrderMark may start a file that an editor wrote.
	byteOrderMark = "\xef\xbb\xbf"

	// maxGitFile caps what originRepo reads from one file, as git caps a .git
	// file.
	maxGitFile = 1 << 20
)

// ownerFunc reports whether path is owned by the current user.
type ownerFunc func(path string) (bool, error)

// originRepo returns OWNER/REPO for the git remote "origin" of the git
// repository that contains dir. It reads the repository's git files itself
// and never runs git. A configuration it does not read (include,
// url.<base>.insteadOf, GIT_DIR, GIT_CONFIG) falls through to an error that
// asks for -R. Like git, it trusts a repository only when the current user
// owns its top directory, its git directory, and the directory of its
// config: a .git that another user planted above the working directory, as
// in a shared /tmp, is refused.
//
// Every error ends with "; pass -R OWNER/REPO", and none names the remote's
// URL, which can hold credentials. A path that .git files supplied is quoted
// in errors, so it cannot write to the terminal.
func originRepo(dir string) (string, error) {
	return originRepoOwned(dir, ownedByCurrentUser)
}

// originRepoOwned is originRepo with the ownership check owned.
func originRepoOwned(dir string, owned ownerFunc) (string, error) {
	repo, err := findOrigin(dir, owned)
	if err != nil {
		return "", fmt.Errorf("%w; %s", err, flagHint)
	}

	return repo, nil
}

func findOrigin(dir string, owned ownerFunc) (string, error) {
	gitDir, err := findGitDir(dir, owned)
	if err != nil {
		return "", err
	}

	// findGitDir checked the top directory and its .git; a .git file can name
	// a git directory elsewhere.
	err = requireOwned(owned, gitDir)
	if err != nil {
		return "", err
	}

	configDir, err := commonDir(gitDir)
	if err != nil {
		return "", err
	}

	err = requireOwned(owned, configDir)
	if err != nil {
		return "", err
	}

	data, err := readFile(filepath.Join(configDir, "config"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", errNoOrigin
	}

	if err != nil {
		return "", fmt.Errorf("read git config: %w", err)
	}

	raw, err := originURL(string(data))
	if err != nil {
		return "", err
	}

	return repoFromURL(raw)
}

// requireOwned fails unless every path is owned by the current user.
func requireOwned(owned ownerFunc, paths ...string) error {
	for _, path := range paths {
		ok, err := owned(path)
		if err != nil {
			return quotePath(err)
		}

		if !ok {
			return fmt.Errorf("%q %w", path, errNotOwned)
		}
	}

	return nil
}

// quotePath returns err with the path of a *fs.PathError quoted. The paths
// that .git files supply can hold terminal escapes. The result still matches
// errors.Is for the cause, such as fs.ErrNotExist.
func quotePath(err error) error {
	pathErr, ok := errors.AsType[*fs.PathError](err)
	if !ok {
		return err
	}

	return fmt.Errorf("%s %q: %w", pathErr.Op, pathErr.Path, pathErr.Err)
}

// readFile reads a file of the repository's git directory. These files are not
// trusted to be what they should: a FIFO would block the read, and a device
// such as /dev/zero would never end, so it reads only a regular file, and
// at most maxGitFile bytes of it.
func readFile(path string) ([]byte, error) {
	// Stat before Open, which blocks on a FIFO.
	info, err := os.Stat(path)
	if err != nil {
		return nil, quotePath(err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q %w", path, errNotRegular)
	}

	//nolint:gosec // G304: the path comes from .git files of a repository that findOrigin checked the user owns.
	file, err := os.Open(path)
	if err != nil {
		return nil, quotePath(err)
	}

	// The file is only read, so a failure to close loses nothing.
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxGitFile+1))
	if err != nil {
		return nil, quotePath(err)
	}

	if len(data) > maxGitFile {
		return nil, fmt.Errorf("%q %w", path, errTooLarge)
	}

	return data, nil
}

// findGitDir returns the git directory of the repository that contains dir.
// It looks for .git in dir and then in each parent, from the physical
// directory, as git does: a symlinked dir leads to the repository of its
// target.
// A .git file, as worktrees and submodules have, names the git directory.
// The first .git found, and the directory that holds it, must be owned by
// the current user.
func findGitDir(dir string, owned ownerFunc) (string, error) {
	if dir == "" {
		return "", errNotInRepo
	}

	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", dir, err)
	}

	cur, err = filepath.EvalSymlinks(cur)
	if err != nil {
		return "", quotePath(err)
	}

	for {
		gitDir, found, err := gitDirIn(cur, owned)
		if err != nil || found {
			return gitDir, err
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return "", errNotInRepo
		}

		cur = parent
	}
}

// gitDirIn returns the git directory that dir's .git is or names, and
// whether dir has a .git.
func gitDirIn(dir string, owned ownerFunc) (string, bool, error) {
	path := filepath.Join(dir, gitFile)

	// Stat, not Lstat: git follows a symlinked .git.
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}

	if err != nil {
		return "", false, quotePath(err)
	}

	// Before reading a .git file: one that another user planted above the
	// working directory, such as /tmp/.git, is never read.
	err = requireOwned(owned, dir, path)
	if err != nil {
		return "", true, err
	}

	switch {
	case info.IsDir():
		return path, true, nil
	case info.Mode().IsRegular():
		gitDir, err := gitDirFromFile(dir, path)

		return gitDir, true, err
	default:
		return "", true, fmt.Errorf("%q: %w: want a directory or a file", path, errMalformedGit)
	}
}

// gitDirFromFile returns the directory named by the .git file at path, which
// is in dir.
func gitDirFromFile(dir, path string) (string, error) {
	data, err := readFile(path)
	if err != nil {
		return "", err
	}

	line, _, _ := strings.Cut(string(data), "\n")

	target, ok := strings.CutPrefix(strings.TrimSpace(line), gitdirPrefix)

	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", fmt.Errorf("%q: %w: want \"gitdir: PATH\"", path, errMalformedGit)
	}

	return resolve(dir, target), nil
}

// resolve returns target as a clean path, relative to base unless it is
// absolute.
func resolve(base, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}

	return filepath.Join(base, target)
}

// commonDir returns the directory that holds the configuration of gitDir:
// gitDir itself, or for a linked worktree the directory its commondir file
// names.
func commonDir(gitDir string) (string, error) {
	path := filepath.Join(gitDir, "commondir")

	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return gitDir, nil
	}

	if err != nil {
		return "", err
	}

	target := strings.TrimSpace(string(data))
	if target == "" {
		return "", fmt.Errorf("%q: %w: empty", path, errMalformedGit)
	}

	return resolve(gitDir, target), nil
}

// originURL returns the first url of the remote "origin" in the git
// configuration text. Section names and keys are case-insensitive;
// subsection names are not. It is errNoOrigin when there is no url, and
// errUnreadable when the first one is in a form it does not read: it never
// falls through to a later url.
func originURL(config string) (string, error) {
	inOrigin := false

	for line := range strings.Lines(strings.TrimPrefix(config, byteOrderMark)) {
		rest := strings.TrimSpace(line)

		if strings.HasPrefix(rest, "[") {
			var ok bool

			inOrigin, rest, ok = parseHeader(rest)
			if !ok {
				inOrigin = false

				continue
			}
		}

		if !inOrigin || isCommentOrEmpty(rest) {
			continue
		}

		// This parser reads one line at a time, so a value that continues
		// onto the next line would make that line look like an entry.
		if continues(rest) {
			return "", errUnreadable
		}

		key, value, ok := parseEntry(rest)
		if !strings.EqualFold(key, "url") {
			continue
		}

		if !ok {
			return "", errUnreadable
		}

		return value, nil
	}

	return "", errNoOrigin
}

// continues reports whether line ends in an unescaped backslash, which joins
// the next line to it.
func continues(line string) bool {
	trailing := len(line) - len(strings.TrimRight(line, "\\"))

	return trailing%2 == 1
}

func isCommentOrEmpty(s string) bool {
	return s == "" || s[0] == '#' || s[0] == ';'
}

// parseHeader parses the section header at the start of s, which begins with
// "[". It reports whether the section is [remote "origin"] and returns what
// follows the header. ok is false for a header it cannot read.
func parseHeader(s string) (bool, string, bool) {
	body := s[1:]

	end := strings.IndexAny(body, " \t]")
	if end < 0 {
		return false, "", false
	}

	section := body[:end]

	body = strings.TrimLeft(body[end:], " \t")
	if body == "" {
		return false, "", false
	}

	if body[0] == ']' {
		return legacyOrigin(section), strings.TrimSpace(body[1:]), true
	}

	if body[0] != '"' {
		return false, "", false
	}

	sub, after, ok := parseQuoted(body[1:])
	if !ok || !strings.HasPrefix(after, "]") {
		return false, "", false
	}

	isOrigin := strings.EqualFold(section, "remote") && sub == "origin"

	return isOrigin, strings.TrimSpace(after[1:]), true
}

// legacyOrigin reports whether section, the text of a header without a quoted
// subsection, is [remote.origin]. Git reads that form with a lowercased
// subsection.
func legacyOrigin(section string) bool {
	name, sub, ok := strings.Cut(section, ".")

	return ok && strings.EqualFold(name, "remote") && strings.EqualFold(sub, "origin")
}

// parseQuoted reads a subsection name up to its closing quote. A backslash
// takes the next character literally, as git does there.
func parseQuoted(text string) (string, string, bool) {
	var name strings.Builder

	for pos := 0; pos < len(text); pos++ {
		switch text[pos] {
		case '"':
			return name.String(), text[pos+1:], true
		case '\\':
			pos++
			if pos == len(text) {
				return "", "", false
			}

			name.WriteByte(text[pos])
		default:
			name.WriteByte(text[pos])
		}
	}

	return "", "", false
}

// parseEntry parses "key = value" or a bare "key". It always returns the key.
// ok is false when text is not an entry with a value this parser can read.
func parseEntry(text string) (string, string, bool) {
	end := strings.IndexAny(text, " \t=")
	if end < 0 {
		return text, "", false
	}

	key := text[:end]

	rest, hasValue := strings.CutPrefix(strings.TrimLeft(text[end:], " \t"), "=")
	if !hasValue {
		return key, "", false
	}

	value, ok := parseValue(strings.TrimLeft(rest, " \t"))

	return key, value, ok
}

// parseValue reads a value as git writes a URL: it ends at a # or ; outside
// double quotes, loses its quotes and surrounding blanks, and resolves the
// escapes \" and \\. ok is false for any other escape or an unclosed quote,
// which leaves origin unread, so the caller asks for -R.
func parseValue(text string) (string, bool) {
	var value strings.Builder

	quoted := false

	for pos := 0; pos < len(text); pos++ {
		switch char := text[pos]; {
		case char == '\\':
			pos++
			if !escapable(text, pos) {
				return "", false
			}

			value.WriteByte(text[pos])
		case char == '"':
			quoted = !quoted
		case !quoted && (char == '#' || char == ';'):
			return strings.TrimSpace(value.String()), true
		default:
			value.WriteByte(char)
		}
	}

	return strings.TrimSpace(value.String()), !quoted
}

// escapable reports whether text[pos], which follows a backslash, is an
// escape parseValue resolves.
func escapable(text string, pos int) bool {
	return pos < len(text) && (text[pos] == '"' || text[pos] == '\\')
}

// repoFromURL returns OWNER/REPO for a GitHub remote URL. Its errors never
// contain raw.
func repoFromURL(raw string) (string, error) {
	if strings.Contains(raw, "://") {
		return repoFromSchemeURL(raw)
	}

	host, path, ok := splitSCP(raw)
	// An scp-like "host" can be a credential (token:x@host:path), so the error
	// does not name it.
	if !ok || !githubHost(host) {
		return "", errNotGitHub
	}

	return ownerRepo(path)
}

// repoFromSchemeURL handles a URL with a scheme.
func repoFromSchemeURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	// err names the URL, so it is dropped.
	if err != nil || !githubScheme(parsed.Scheme) {
		return "", errNotGitHub
	}

	// url.Parse took the userinfo off, so the host is safe to name.
	if host := parsed.Hostname(); !githubHost(host) {
		return "", hostError(host)
	}

	return ownerRepo(parsed.Path)
}

// githubScheme reports whether scheme is one git uses to reach GitHub.
func githubScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		return true
	default:
		return false
	}
}

func githubHost(host string) bool {
	return strings.EqualFold(host, "github.com") || strings.EqualFold(host, "ssh.github.com")
}

// hostError reports that the remote is on host. A host is no credential,
// unlike the rest of the URL.
func hostError(host string) error {
	if host == "" {
		return errNotGitHub
	}

	return fmt.Errorf("%w (it is on %q)", errNotGitHub, host)
}

// splitSCP splits git's scp-like syntax [user@]host:path. It applies when
// there is a colon and no slash before it.
func splitSCP(raw string) (string, string, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || strings.Contains(raw[:colon], "/") {
		return "", "", false
	}

	host := raw[:colon]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}

	return host, raw[colon+1:], true
}

// ownerRepo returns OWNER/REPO from the path of a GitHub URL, if the names
// are ones GitHub allows. ValidRepo's character set also keeps control and
// escape characters out of the notice that names the repository.
func ownerRepo(path string) (string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")

	if !sandbox.ValidRepo(path) {
		return "", errBadRemote
	}

	return path, nil
}
