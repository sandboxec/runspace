package sandbox

import "time"

// IdleClose is how long a cached Conn may go without a call before it is
// closed.
const IdleClose = idleClose

// Forget closes and evicts the cached Conn of name, as delete does.
func (s *Sandboxes) Forget(name string) { s.forget(name) }

// Cached reports whether a Conn, or a dial in progress, is cached for name.
func (s *Sandboxes) Cached(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.conns[name]

	return ok
}

// ParsedIdle is the idle timeout Validate settled on.
func (o *CreateOptions) ParsedIdle() time.Duration { return o.idle }

// ParsedRetention is the retention Validate settled on.
func (o *CreateOptions) ParsedRetention() time.Duration { return o.retention }

// ParsedTimeout is the command timeout Validate settled on.
func (o *TimeoutOption) ParsedTimeout() time.Duration { return o.timeout }
