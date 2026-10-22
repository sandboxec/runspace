//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// failsFast runs originRepo on dir and fails the test if it blocks or
// succeeds.
func failsFast(t *testing.T, dir string, want error) {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		_, err := originRepo(dir)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("originRepo error = %v, want %v", err, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("originRepo blocked")
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()

	err := syscall.Mkfifo(path, 0o600)
	if err != nil {
		t.Skipf("no FIFOs: %v", err)
	}
}

// A FIFO where the config or commondir should be would block a read forever.
func TestOriginRepoFIFO(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"config", "commondir"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".git", "config"), originConfig(githubOrigin))

			path := filepath.Join(root, ".git", name)
			if name == "config" {
				err := os.Remove(path)
				if err != nil {
					t.Fatal(err)
				}
			}

			mkfifo(t, path)
			failsFast(t, root, errNotRegular)
		})
	}
}

func TestOriginRepoFIFOAsGitFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mkfifo(t, filepath.Join(root, ".git"))
	failsFast(t, root, errMalformedGit)
}

// A device such as /dev/zero would grow without end.
func TestOriginRepoDevice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "")

	err := os.Symlink("/dev/zero", filepath.Join(root, ".git", "config"))
	if err != nil {
		t.Skipf("no symlinks: %v", err)
	}

	failsFast(t, root, errNotRegular)
}

func TestOriginRepoOversizedConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"),
		originConfig(githubOrigin)+"# "+strings.Repeat("x", maxGitFile)+"\n")
	failsFast(t, root, errTooLarge)
}

func TestOriginRepoConfigAtTheCap(t *testing.T) {
	t.Parallel()

	head := originConfig(githubOrigin) + "# "
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), head+strings.Repeat("x", maxGitFile-len(head)))

	got, err := originRepo(root)
	if err != nil || got != repoName {
		t.Errorf("originRepo = %q, %v; want %s", got, err, repoName)
	}
}

func TestOwnedByCurrentUser(t *testing.T) {
	t.Parallel()

	owned, err := ownedByCurrentUser(t.TempDir())
	if err != nil || !owned {
		t.Errorf("ownedByCurrentUser(temp dir) = %v, %v; want true", owned, err)
	}

	if os.Geteuid() != 0 {
		owned, err = ownedByCurrentUser("/")
		if err != nil || owned {
			t.Errorf("ownedByCurrentUser(/) = %v, %v; want false", owned, err)
		}
	}

	_, err = ownedByCurrentUser(filepath.Join(t.TempDir(), "gone"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ownedByCurrentUser(missing) error = %v", err)
	}
}
