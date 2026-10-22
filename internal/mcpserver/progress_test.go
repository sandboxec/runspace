//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver_test

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.sandbox.ec/runspace/internal/mcpserver"
)

// stop waits for a report in flight, so no progress notification follows
// the tool result.
func TestHeartbeatStopWaitsForReport(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		t.Helper()

		var reports atomic.Int32

		release := make(chan struct{})
		stop := mcpserver.Heartbeat(func(string) {
			reports.Add(1)
			<-release
		})

		time.Sleep(mcpserver.HeartbeatEvery)
		synctest.Wait()

		if got := reports.Load(); got != 1 {
			t.Fatalf("reports after one period = %d, want 1", got)
		}

		stopped := make(chan struct{})

		go func() {
			stop()
			close(stopped)
		}()

		synctest.Wait()

		select {
		case <-stopped:
			t.Fatal("stop returned while a report was in flight")
		default:
		}

		close(release)
		<-stopped

		time.Sleep(2 * mcpserver.HeartbeatEvery)
		synctest.Wait()

		if got := reports.Load(); got != 1 {
			t.Errorf("reports after stop = %d, want 1", got)
		}
	})
}

// A nil report starts nothing, and its stop does nothing.
func TestHeartbeatNilReport(t *testing.T) {
	t.Parallel()

	mcpserver.Heartbeat(nil)()
}
