//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.sandbox.ec/runspace/internal/remote"
)

// diffScript prints the patch from refs/runspace/base to the working tree,
// covering commits, uncommitted edits, and new files. It stages into a copy
// of the index, so the real index is never touched, and it pins every
// setting that changes the output, so the sandbox's git configuration
// cannot.
const diffScript = `set -eu
base=$(git rev-parse --verify --quiet '` + BaseRef + `^{commit}') || {
	echo "` + BaseRef + ` is missing; runspace create records it" >&2
	exit 1
}
index=$(mktemp)
trap 'rm -f "$index" "$index.lock"' EXIT
real=$(git rev-parse --git-path index)
if [ -f "$real" ]; then cp "$real" "$index"; else rm -f "$index"; fi
export GIT_INDEX_FILE="$index" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git -c core.autocrlf=false -c core.safecrlf=false -c core.fileMode=true \
	-c core.fsmonitor=false -c core.symlinks=true add -A
git -c diff.noprefix=false -c diff.mnemonicPrefix=false -c core.quotePath=true \
	diff --cached --binary --no-ext-diff --no-textconv --no-color --no-renames \
	--no-relative --src-prefix=a/ --dst-prefix=b/ -U3 --inter-hunk-context=0 \
	--diff-algorithm=myers --indent-heuristic -O/dev/null "$base"`

// Diff writes the sandbox's patch to streams.Stdout, or atomically to
// opts.Output. It returns the absolute path written, or "".
func (s *Sandboxes) Diff(ctx context.Context, opts DiffOptions, streams Streams) (string, error) {
	err := opts.Validate()
	if err != nil {
		return "", err
	}

	if opts.Output == "" {
		return "", s.diff(ctx, &opts, streams)
	}

	file, err := createAtomic(opts.Output)
	if err != nil {
		return "", err
	}
	defer file.Abort()

	streams.Stdout = file

	err = s.diff(ctx, &opts, streams)
	if err != nil {
		return "", err
	}

	err = file.Commit()
	if err != nil {
		return "", err
	}

	return file.path, nil
}

// diff runs diffScript and writes its output to streams.Stdout.
func (s *Sandboxes) diff(ctx context.Context, opts *DiffOptions, streams Streams) error {
	held, err := s.acquire(ctx, opts.Name, streams.progress())
	if err != nil {
		return fmt.Errorf("diff %s: %w", opts.Name, err)
	}

	stderr := new(capped)

	code, err := held.run(ctx, &opts.TimeoutOption, "the diff", remote.Cmd{
		Dir:    held.workspace,
		Env:    nil,
		Argv:   scriptArgv(diffScript),
		Stdin:  nil,
		Stdout: streams.Stdout,
		Stderr: stderr,
		TTY:    nil,
	})

	switch {
	case err != nil:
		return fmt.Errorf("diff %s: %w", opts.Name, err)
	case code != 0:
		return fmt.Errorf("diff %s: %w", opts.Name, exited(errExit, code, stderr))
	}

	return nil
}

// atomicFile is written in a temporary file next to its target and renamed
// into place by Commit, so the target is never partial.
type atomicFile struct {
	*os.File

	path string
	done bool
}

func createAtomic(path string) (*atomicFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	file, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".runspace-*")
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}

	return &atomicFile{File: file, path: abs, done: false}, nil
}

// Commit closes the file and renames it to its target.
func (a *atomicFile) Commit() error {
	a.done = true

	err := a.Close()
	if err == nil {
		err = os.Rename(a.Name(), a.path)
	}

	if err != nil {
		// Best effort: the write already failed.
		_ = os.Remove(a.Name())

		return fmt.Errorf("write %s: %w", a.path, err)
	}

	return nil
}

// Abort removes the temporary file unless Commit ran.
func (a *atomicFile) Abort() {
	if a.done {
		return
	}

	a.done = true
	// Best effort: nothing can act on a failure to clean up.
	_ = a.Close()
	_ = os.Remove(a.Name())
}

var _ io.Writer = (*atomicFile)(nil)
