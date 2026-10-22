package sandbox

import (
	"bytes"
	"fmt"
	"strings"
)

// cappedSize bounds output that runspace captures from a sandbox for its
// own messages.
const cappedSize = 64 << 10

// capped keeps the first cappedSize bytes written to it and discards the
// rest, so output from an untrusted sandbox cannot exhaust memory. The zero
// value is ready to use.
type capped struct {
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := cappedSize - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// exited is the error for a runspace script that exited with a non-zero
// code, and what it wrote to stderr.
func exited(kind error, code int, stderr *capped) error {
	return fmt.Errorf("%w %d: %s", kind, code, strings.TrimSpace(stderr.String()))
}
