//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
)

// streams guards the caller's streams for one session. It records their
// first failure, and once detached it keeps a session that outlives Exec,
// such as one whose peer never confirms a close, from starting further reads
// or writes on them.
type streams struct {
	mu       sync.Mutex
	err      error
	detached bool
}

// attach connects the streams of cmd to sess through guards. A nil Stdin
// makes the session send EOF at once.
func (s *streams) attach(sess *ssh.Session, cmd Cmd) {
	if cmd.Stdin != nil {
		sess.Stdin = watchedReader{r: cmd.Stdin, s: s}
	}

	if cmd.Stdout != nil {
		sess.Stdout = watchedWriter{w: cmd.Stdout, s: s}
	}

	if cmd.Stderr != nil {
		sess.Stderr = watchedWriter{w: cmd.Stderr, s: s}
	}
}

func (s *streams) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// detach makes every later read return EOF and every later write vanish.
// It waits for a write in progress; a read already in progress may still
// finish afterwards.
func (s *streams) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.detached = true
}

// recordLocked keeps the first failure. The caller holds s.mu.
func (s *streams) recordLocked(err error) {
	if s.err == nil {
		s.err = err
	}
}

type watchedWriter struct {
	w io.Writer
	s *streams
}

func (w watchedWriter) Write(data []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()

	if w.s.detached {
		return len(data), nil
	}

	written, err := w.w.Write(data)
	if err != nil {
		w.s.recordLocked(err)

		return written, fmt.Errorf("%w", err)
	}

	return written, nil
}

type watchedReader struct {
	r io.Reader
	s *streams
}

// Read does not hold the lock while it blocks on the caller's reader, which
// would stall output; a read that started before detach may still finish.
func (r watchedReader) Read(data []byte) (int, error) {
	r.s.mu.Lock()
	detached := r.s.detached
	r.s.mu.Unlock()

	if detached {
		return 0, io.EOF
	}

	count, err := r.r.Read(data)

	switch err {
	case nil:
		return count, nil
	case io.EOF: // The reader's end of input, which io.Copy compares by identity.
		return count, io.EOF
	default:
		r.s.mu.Lock()
		r.s.recordLocked(err)
		r.s.mu.Unlock()

		return count, fmt.Errorf("%w", err)
	}
}
