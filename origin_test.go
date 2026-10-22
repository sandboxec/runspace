package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoFromURLAccepts(t *testing.T) {
	t.Parallel()

	urls := []string{
		"https://github.com/o/r.git",
		"https://github.com/o/r",
		"https://github.com/o/r/",
		"https://github.com/o/r.git/",
		"http://github.com/o/r.git",
		"https://user:SECRET@github.com/o/r.git",
		"git@github.com:o/r.git",
		"github.com:o/r",
		"ssh://git@github.com/o/r.git",
		"ssh://git@ssh.github.com:443/o/r.git",
		"git://github.com/o/r.git",
		"git+ssh://git@github.com/o/r.git",
		"ssh+git://git@github.com/o/r.git",
		"https://GitHub.com/o/r.git",
		"git@SSH.GitHub.com:o/r.git",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			got, err := repoFromURL(raw)
			if err != nil || got != repoName {
				t.Errorf("repoFromURL(%q) = %q, %v; want %s", raw, got, err, repoName)
			}
		})
	}
}

func TestRepoFromURLRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want error
	}{
		{"https://gitlab.com/o/r.git", errNotGitHub},
		{"git@gitlab.com:o/r.git", errNotGitHub},
		{"https://github.com.evil.example/o/r.git", errNotGitHub},
		{"/srv/repo.git", errNotGitHub},
		{"file:///srv/repo.git", errNotGitHub},
		{"ftp://github.com/o/r.git", errNotGitHub},
		{"", errNotGitHub},
		{"https://github.com/r.git", errBadRemote},
		{"https://github.com/o/r/x.git", errBadRemote},
		{"https://github.com/", errBadRemote},
		{"https://github.com//r", errBadRemote},
		{"git@github.com:r", errBadRemote},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			t.Parallel()

			got, err := repoFromURL(test.raw)
			if !errors.Is(err, test.want) {
				t.Errorf("repoFromURL(%q) = %q, %v; want %v", test.raw, got, err, test.want)
			}
		})
	}
}

// No error repeats the URL or any part of it, credentials above all.
func TestRepoFromURLErrorsHideTheURL(t *testing.T) {
	t.Parallel()

	urls := []string{
		"https://user:SECRET@gitlab.com/o/r.git",
		"https://user:SECRET@github.com/one",
		"https://user:SECRET@github.com/a/b/c",
		"ssh://user:SECRET@example.org/o/r",
		"ftp://user:SECRET@github.com/o/r",
		"user:SECRET@gitlab.com:o/r",
		"https://user:SECRET@%zz/o/r",
		"SECRET@gitlab.com:o/r",
		"/srv/SECRET/repo.git",
		"ghp_SECRET:x-oauth-basic@gitlab.com:o/r",
		"ghp_SECRET:x-oauth-basic@github.com:o/r/x",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			_, err := repoFromURL(raw)
			if err == nil {
				t.Fatal("no error")
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error leaks the credential: %v", err)
			}
		})
	}
}

type originURLCase struct {
	name   string
	config string
	want   string
	err    error
}

func originURLCases() []originURLCase {
	return []originURLCase{
		{"plain", "[remote \"origin\"]\n\turl = u1\n", "u1", nil},
		{"section is case-insensitive", "[REMOTE \"origin\"]\nurl = u1\n", "u1", nil},
		{"key is case-insensitive", "[remote \"origin\"]\n\tURL = u1\n", "u1", nil},
		{"subsection is case-sensitive", "[remote \"Origin\"]\nurl = u1\n", "", errNoOrigin},
		{"legacy header", "[remote.origin]\nurl = u1\n", "u1", nil},
		{"legacy header is case-insensitive", "[Remote.Origin]\nurl = u1\n", "u1", nil},
		{"legacy header of another remote", "[remote.up]\nurl = u1\n", "", errNoOrigin},
		{"other remote first", "[remote \"up\"]\n\turl = u0\n[remote \"origin\"]\n\turl = u1\n", "u1", nil},
		{"other section keys", "[branch \"main\"]\n\turl = u0\n[remote \"origin\"]\n\tfetch = f\n\turl = u1\n", "u1", nil},
		{"first url wins", "[remote \"origin\"]\n\turl = u1\n\turl = u2\n", "u1", nil},
		{"origin after leaving it", "[remote \"origin\"]\n\tfetch = f\n[core]\n\turl = u0\n", "", errNoOrigin},
		{"no origin", "[core]\n\tbare = false\n", "", errNoOrigin},
		{"empty", "", "", errNoOrigin},
		{"whole-line comments", "# c\n; c\n[remote \"origin\"]\n# url = u0\n\t; url = u0\n\turl = u1\n", "u1", nil},
		{"comment after header", "[remote \"origin\"] # c\n\turl = u1\n", "u1", nil},
		{"entry on the header line", "[remote \"origin\"] url = u1\n", "u1", nil},
		{"trailing comment", "[remote \"origin\"]\n\turl = u1 # c\n", "u1", nil},
		{"trailing semicolon comment", "[remote \"origin\"]\n\turl = u1; c\n", "u1", nil},
		{"no spaces around equals", "[remote \"origin\"]\nurl=u1\n", "u1", nil},
		{"surrounding whitespace", "  [remote \"origin\"]  \n  url   =   u1  \t\n", "u1", nil},
		{"quoted value", "[remote \"origin\"]\n\turl = \"u 1\"\n", "u 1", nil},
		{"quote hides comment", "[remote \"origin\"]\n\turl = \"u#1;2\" # c\n", "u#1;2", nil},
		{"escapes", "[remote \"origin\"]\n\turl = \"a\\\"b\\\\c\"\n", "a\"b\\c", nil},
		{"other escape", "[remote \"origin\"]\n\turl = \"a\\nb\"\n", "", errUnreadable},
		{"inner space is kept", "[remote \"origin\"]\n\turl = a  b\n", "a  b", nil},
		{"unknown escape", "[remote \"origin\"]\n\turl = a\\qb\n", "", errUnreadable},
		{"unclosed quote", "[remote \"origin\"]\n\turl = \"a\n", "", errUnreadable},
		{"key without value", "[remote \"origin\"]\n\turl\n", "", errUnreadable},
		{"quote in subsection", "[remote \"or\\\"igin\"]\nurl = u0\n", "", errNoOrigin},
		{"unclosed header", "[remote \"origin\"\nurl = u0\n", "", errNoOrigin},
		{"unknown escape, then a valid url", "[remote \"origin\"]\n\turl = a\\qb\n\turl = u2\n", "", errUnreadable},
		{"unclosed quote, then a valid url", "[remote \"origin\"]\n\turl = \"a\n\turl = u2\n", "", errUnreadable},
		{"bare key, then a second one", "[remote \"origin\"]\n\turl\n\tfetch = f\n\turl = u2\n", "", errUnreadable},
		{"continuation, then a valid url", "[remote \"origin\"]\n\turl = u1\\\n\turl = u2\n", "", errUnreadable},
		{
			"continuation of another key", "[remote \"origin\"]\n\tnote = a \\\n\turl = evil\n\turl = u2\n",
			"", errUnreadable,
		},
		{"escaped backslash at the end", "[remote \"origin\"]\n\tnote = a\\\\\n\turl = u1\n", "u1", nil},
		{"CRLF", "[remote \"origin\"]\r\n\turl = u1\r\n", "u1", nil},
		{"BOM", "\xef\xbb\xbf[remote \"origin\"]\n\turl = u1\n", "u1", nil},
		{"no trailing newline", "[remote \"origin\"]\n\turl = u1", "u1", nil},
	}
}

func TestOriginURL(t *testing.T) {
	t.Parallel()

	for _, test := range originURLCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := originURL(test.config)
			if got != test.want || !errors.Is(err, test.err) {
				t.Errorf("originURL = %q, %v; want %q, %v", got, err, test.want, test.err)
			}
		})
	}
}

// writeFile writes content to path, making its directory.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// originConfig is a git config whose remote origin is url.
func originConfig(url string) string {
	return "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + url + "\n"
}

const (
	githubOrigin = "https://github.com/o/r.git"
	gitConfig    = gitFile + "/config"
)

func TestOriginRepoFindsTheRepository(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), originConfig(githubOrigin))

	deep := filepath.Join(root, "a", "b")

	err := os.MkdirAll(deep, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{root, deep} {
		got, err := originRepo(dir)
		if err != nil || got != repoName {
			t.Errorf("originRepo(%s) = %q, %v; want %s", dir, got, err, repoName)
		}
	}
}

// The nearest .git wins, so a repository nested in another uses its own
// origin.
func TestOriginRepoNearestRepositoryWins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inner := filepath.Join(root, "inner")

	writeFile(t, filepath.Join(root, ".git", "config"), originConfig("https://github.com/outer/repo.git"))
	writeFile(t, filepath.Join(inner, ".git", "config"), originConfig(githubOrigin))

	got, err := originRepo(inner)
	if err != nil || got != repoName {
		t.Errorf("originRepo = %q, %v; want %s", got, err, repoName)
	}
}

// A linked worktree: its .git file names a directory under the main
// repository's .git, whose commondir leads back to the main repository's
// config.
func TestOriginRepoWorktree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	main := filepath.Join(root, "main")
	tree := filepath.Join(root, "wt")
	treeGit := filepath.Join(main, ".git", "worktrees", "wt")

	writeFile(t, filepath.Join(main, ".git", "config"), originConfig(githubOrigin))
	writeFile(t, filepath.Join(treeGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(tree, ".git"), "gitdir: "+treeGit+"\n")

	got, err := originRepo(tree)
	if err != nil || got != repoName {
		t.Errorf("originRepo = %q, %v; want %s", got, err, repoName)
	}
}

func TestOriginRepoAbsoluteCommondir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	common := filepath.Join(root, "common")
	tree := filepath.Join(root, "wt")

	writeFile(t, filepath.Join(common, "config"), originConfig(githubOrigin))
	writeFile(t, filepath.Join(root, "gitdir", "commondir"), common+"\n")
	writeFile(t, filepath.Join(tree, ".git"), "gitdir: "+filepath.Join(root, "gitdir"))

	got, err := originRepo(tree)
	if err != nil || got != repoName {
		t.Errorf("originRepo = %q, %v; want %s", got, err, repoName)
	}
}

// A submodule: its .git file names a relative directory with its own config.
func TestOriginRepoSubmodule(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sub := filepath.Join(root, "sub")

	writeFile(t, filepath.Join(root, ".git", "modules", "sub", "config"), originConfig(githubOrigin))
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")

	got, err := originRepo(sub)
	if err != nil || got != repoName {
		t.Errorf("originRepo = %q, %v; want %s", got, err, repoName)
	}
}

func TestOriginRepoErrors(t *testing.T) {
	t.Parallel()

	secretOrigin := originConfig("https://user:SECRET@gitlab.com/o/r.git")

	tests := []struct {
		name  string
		files map[string]string // content by path under the directory
		want  error
	}{
		{"not a repository", nil, errNotInRepo},
		{"no config", map[string]string{".git/HEAD": "ref: refs/heads/main\n"}, errNoOrigin},
		{"no origin", map[string]string{gitConfig: "[remote \"up\"]\n\turl = https://github.com/o/r\n"}, errNoOrigin},
		{"origin without url", map[string]string{gitConfig: "[remote \"origin\"]\n\tfetch = f\n"}, errNoOrigin},
		{"not on GitHub", map[string]string{gitConfig: secretOrigin}, errNotGitHub},
		{"wrong shape", map[string]string{gitConfig: originConfig("https://github.com/o/r/x")}, errBadRemote},
		{"malformed .git file", map[string]string{gitFile: "this is not a gitdir line\n"}, errMalformedGit},
		{"empty .git file", map[string]string{gitFile: ""}, errMalformedGit},
		{"gitdir without a path", map[string]string{gitFile: "gitdir:  \n"}, errMalformedGit},
		{"unreadable url", map[string]string{gitConfig: originConfig("a\\qb")}, errUnreadable},
		{"origin with an escape", map[string]string{gitConfig: originConfig("https://github.com/o%1b%5b2J/r")}, errBadRemote},
		{"empty commondir", map[string]string{".git/commondir": "\n"}, errMalformedGit},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()

			for path, content := range test.files {
				writeFile(t, filepath.Join(root, path), content)
			}

			// The temporary directory's parents are not repositories.
			got, err := originRepo(root)
			if !errors.Is(err, test.want) || got != "" {
				t.Fatalf("originRepo = %q, %v; want %v", got, err, test.want)
			}

			if !strings.HasSuffix(err.Error(), "; "+flagHint) {
				t.Errorf("error does not say to pass -R: %v", err)
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error leaks the credential: %v", err)
			}

			if strings.Contains(err.Error(), "\x1b") {
				t.Errorf("error holds a raw escape: %q", err)
			}
		})
	}
}

// A symlinked .git is followed, as git does.
func TestOriginRepoSymlinkedGit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "real", "config"), originConfig(githubOrigin))

	err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, ".git"))
	if err != nil {
		t.Skipf("no symlinks: %v", err)
	}

	repo, err := originRepo(root)
	if repo != "o/r" || err != nil {
		t.Errorf("originRepo = %q, %v; want o/r", repo, err)
	}
}

func TestOriginRepoNoDirectory(t *testing.T) {
	t.Parallel()

	_, err := originRepo("")
	if !errors.Is(err, errNotInRepo) {
		t.Errorf("originRepo(\"\") error = %v, want %v", err, errNotInRepo)
	}
}

// An unreadable config is reported with its path, not as a missing origin.
func TestOriginRepoUnreadableConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// A directory where the config file should be.
	writeFile(t, filepath.Join(root, ".git", "config", "x"), "")

	_, err := originRepo(root)
	if err == nil || errors.Is(err, errNoOrigin) || !strings.Contains(err.Error(), "read git config") {
		t.Errorf("originRepo error = %v", err)
	}
}

// Real git, when it is installed, writes the layouts above; the parser
// agrees with it on a worktree and on a submodule.
func TestOriginRepoRealGit(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	main := filepath.Join(root, "main")
	tree := filepath.Join(root, "wt")

	git := func(dir string, args ...string) {
		t.Helper()

		//nolint:gosec // G204: the test runs the git it found on PATH, with arguments of its own.
		cmd := exec.CommandContext(t.Context(), gitPath, args...)
		cmd.Dir = dir

		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}

	err = os.MkdirAll(main, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	git(main, "init", "-q")
	git(main, "remote", "add", "origin", "git@github.com:o/r.git")
	git(main, "commit", "-q", "--allow-empty", "-m", "x")
	git(main, "worktree", "add", "-q", tree)

	// A submodule whose own origin is on GitHub, added from a local source.
	lib := filepath.Join(root, "lib")

	err = os.MkdirAll(lib, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	git(lib, "init", "-q")
	git(lib, "commit", "-q", "--allow-empty", "-m", "x")
	git(main, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "sub")

	module := filepath.Join(main, "sub")
	git(module, "remote", "set-url", "origin", "git@github.com:o/sub.git")

	for dir, want := range map[string]string{main: repoName, tree: repoName, module: "o/sub"} {
		got, err := originRepo(dir)
		if err != nil || got != want {
			t.Errorf("originRepo(%s) = %q, %v; want %s", dir, got, err, want)
		}
	}
}

// A symlinked working directory is resolved first, as git does, so the
// repository is the target's, not the one above the link.
func TestOriginRepoPhysicalDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "data", "proj", "pkg")

	err := os.MkdirAll(target, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "data", "proj", ".git", "config"), originConfig(githubOrigin))
	writeFile(t, filepath.Join(root, "home", ".git", "config"), originConfig("https://github.com/home/dotfiles.git"))

	link := filepath.Join(root, "home", "work")

	err = os.Symlink(target, link)
	if err != nil {
		t.Skipf("no symlinks: %v", err)
	}

	got, err := originRepo(link)
	if err != nil || got != repoName {
		t.Errorf("originRepo(%s) = %q, %v; want %s", link, got, err, repoName)
	}
}

func TestOriginRepoMissingDirectory(t *testing.T) {
	t.Parallel()

	_, err := originRepo(filepath.Join(t.TempDir(), "gone\x1b[2J"))
	if !errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("originRepo error = %q", err)
	}
}

// A gitdir target with a terminal escape never reaches an error raw.
func TestOriginRepoQuotesGitdirTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git"), "gitdir: "+filepath.Join(root, "gone\x1b[2J")+"\n")

	_, err := originRepo(root)
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("originRepo error = %q", err)
	}
}

// A commondir with a terminal escape never reaches an error raw either.
func TestOriginRepoQuotesCommondir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "commondir"), "gone\x1b[2J\n")

	_, err := originRepo(root)
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("originRepo error = %q", err)
	}
}

// ownedExcept returns an ownerFunc that reports each path as owned except
// the named one, and records nothing.
func ownedExcept(other string) ownerFunc {
	return func(path string) (bool, error) { return path != other, nil }
}

// A .git above the working directory that another user owns is refused
// before it is read: its malformed content is never looked at.
func TestOriginRepoUnownedGitFileIsNotRead(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, ".git"), "not a gitdir line\n")

	work := filepath.Join(root, "work")

	err = os.MkdirAll(work, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	_, err = originRepoOwned(work, ownedExcept(root))
	if !errors.Is(err, errNotOwned) || errors.Is(err, errMalformedGit) {
		t.Errorf("originRepoOwned error = %v, want %v", err, errNotOwned)
	}
}

// The repository's top directory, git directory, and config directory
// must each be owned by the current user.
func TestOriginRepoOwnership(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	main := filepath.Join(root, "main")
	tree := filepath.Join(main, "wt")
	treeGit := filepath.Join(main, ".git", "worktrees", "wt")

	writeFile(t, filepath.Join(main, ".git", "config"), originConfig(githubOrigin))
	writeFile(t, filepath.Join(treeGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(tree, ".git"), "gitdir: "+treeGit+"\n")

	deep := filepath.Join(tree, "a", "b")

	err = os.MkdirAll(deep, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	got, err := originRepoOwned(deep, ownedExcept(""))
	if err != nil || got != repoName {
		t.Fatalf("originRepoOwned = %q, %v; want %s", got, err, repoName)
	}

	// The worktree's top is tree; its git directory is treeGit; its config is
	// in the main repository's .git.
	disowned := map[string]string{"top": tree, "git dir": treeGit, "config dir": filepath.Join(main, ".git")}

	for name, other := range disowned {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := originRepoOwned(deep, ownedExcept(other))
			if got != "" || !errors.Is(err, errNotOwned) {
				t.Fatalf("originRepoOwned = %q, %v; want %v", got, err, errNotOwned)
			}

			want := fmt.Sprintf("%q is owned by another user; %s", other, flagHint)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}
}

// A .git that another user planted above the working directory is refused.
func TestOriginRepoPlantedAbove(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, ".git", "config"), originConfig(githubOrigin))

	work := filepath.Join(root, "home", "me")

	err = os.MkdirAll(work, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	_, err = originRepoOwned(work, ownedExcept(root))
	if !errors.Is(err, errNotOwned) || !strings.Contains(err.Error(), strconv.Quote(root)) {
		t.Errorf("originRepoOwned error = %v", err)
	}
}

var errLookup = errors.New("lookup failed")

func TestOriginRepoOwnerLookupFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), originConfig(githubOrigin))

	_, err := originRepoOwned(root, func(string) (bool, error) { return false, errLookup })
	if !errors.Is(err, errLookup) {
		t.Errorf("originRepoOwned error = %v, want %v", err, errLookup)
	}
}
