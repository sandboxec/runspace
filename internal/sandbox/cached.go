package sandbox

import (
	"context"
	"fmt"
	"time"
)

// cached is a shared Conn and the dial that produces it.
//
// Invariants, under Sandboxes.mu unless noted:
//   - every acquire, including its failure paths, is paired with exactly
//     one release, which keeps inflight balanced;
//   - release and adopt arm idle; acquire and forget disarm it;
//   - session and err are written once, before ready is closed, and read
//     only after it;
//   - an entry leaves conns before its Conn is closed. More than one call
//     may close that Conn: release closes it after a transport error even
//     when the entry already left conns, so Conn.Close must allow repeated
//     calls.
type cached struct {
	session

	ready  chan struct{} // closed when the dial ends
	cancel context.CancelFunc
	err    error

	// Guarded by Sandboxes.mu.
	inflight int
	idle     *time.Timer
}

// dead reports whether e holds a Conn that is already closed.
func (e *cached) dead() bool {
	select {
	case <-e.ready:
	default:
		return false
	}

	if e.conn == nil {
		return false
	}

	select {
	case <-e.conn.Done():
		return true
	default:
		return false
	}
}

// close cancels the dial if it is still running, waits for it, and closes
// its Conn, if it produced one.
func (e *cached) close() error {
	if e.cancel != nil {
		e.cancel()
	}

	<-e.ready

	if e.conn == nil {
		return nil
	}

	err := e.conn.Close()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// discard is close for an entry that already left the cache: nobody can act
// on a failure to close, so it is dropped.
func (e *cached) discard() {
	_ = e.close()
}
