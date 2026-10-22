//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"context"
	"net"

	"golang.org/x/crypto/ssh"
)

// Wrapper is the shell script CommandLine sends.
const Wrapper = wrapper

// Timing is the set of intervals of one Conn, for tests that shorten them.
type Timing = timing

// DefaultTiming returns the production intervals.
func DefaultTiming() Timing { return defaultTiming() }

// NewConn is newConn with test-set intervals.
func NewConn(
	ctx context.Context,
	tunnel net.Conn,
	user string,
	signer ssh.Signer,
	intervals Timing,
	teardown ...func() error,
) (*Conn, error) {
	return newConn(ctx, tunnel, user, signer, intervals, teardown...)
}
