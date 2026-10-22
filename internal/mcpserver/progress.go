//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// heartbeatEvery is how often a running command reports progress.
const heartbeatEvery = time.Minute

// progress returns a function that sends progress notifications for req,
// or nil when the client sent no progress token. It is safe for concurrent
// use.
func progress(ctx context.Context, req *mcp.CallToolRequest) func(string) {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}

	var lock sync.Mutex

	sent := 0

	return func(message string) {
		lock.Lock()
		defer lock.Unlock()

		sent++

		params := &mcp.ProgressNotificationParams{
			Meta:          nil,
			ProgressToken: token,
			Message:       message,
			Progress:      float64(sent),
			Total:         0,
		}
		// A failed notification must not fail the call.
		_ = req.Session.NotifyProgress(ctx, params)
	}
}

// heartbeat reports progress every heartbeatEvery until the returned stop
// is called, so a client that times out quiet calls keeps a long call alive.
// A nil report sends nothing. Once stop returns, report is not called again.
func heartbeat(report func(string)) func() {
	if report == nil {
		return func() {}
	}

	done := make(chan struct{})
	exited := make(chan struct{})

	go func() {
		defer close(exited)

		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()

		start := time.Now()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				report("working for " + time.Since(start).Round(time.Second).String())
			}
		}
	}()

	return func() {
		close(done)
		<-exited
	}
}
