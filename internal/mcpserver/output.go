package mcpserver

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

// outputCapKiB is outputCap in KiB, as the errors state it. The descriptions
// hard-code the figure, and a test keeps them in step.
const outputCapKiB = 32

// outputCap bounds each stream an MCP result carries.
const outputCap = outputCapKiB << 10

// tail keeps the last outputCap bytes written to it and counts the rest, so
// a command's output cannot exhaust memory or the client's context.
type tail struct {
	ring    []byte
	start   int // index of the oldest byte once ring is full
	dropped int64
}

func newTail() *tail {
	return &tail{ring: make([]byte, 0, outputCap), start: 0, dropped: 0}
}

func (t *tail) Write(data []byte) (int, error) {
	written := len(data)

	if len(data) > outputCap {
		t.dropped += int64(len(data) - outputCap)
		data = data[len(data)-outputCap:]
	}

	if room := outputCap - len(t.ring); room > 0 {
		fill := min(room, len(data))
		t.ring = append(t.ring, data[:fill]...)
		data = data[fill:]
	}

	for len(data) > 0 {
		overwritten := copy(t.ring[t.start:], data)
		t.dropped += int64(overwritten)
		t.start = (t.start + overwritten) % outputCap
		data = data[overwritten:]
	}

	return written, nil
}

// Truncated reports whether any output was dropped.
func (t *tail) Truncated() bool { return t.dropped > 0 }

// String returns the kept output, prefixed with a [truncated N bytes] marker
// when the start was dropped. A rune cut by the drop is skipped.
func (t *tail) String() string {
	kept := append(bytes.Clone(t.ring[t.start:]), t.ring[:t.start]...)
	if t.dropped == 0 {
		return string(kept)
	}

	skipped := 0
	for skipped < utf8.UTFMax && skipped < len(kept) && !utf8.RuneStart(kept[skipped]) {
		skipped++
	}

	return fmt.Sprintf("[truncated %d bytes]\n", t.dropped+int64(skipped)) + string(kept[skipped:])
}

// boundedBuffer keeps up to outputCap bytes and records whether more
// arrived. It holds its buffer in a field, not embedded, so that
// io.WriteString cannot reach an uncapped WriteString.
type boundedBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)

	if room := outputCap - b.buf.Len(); written > room {
		b.overflow = true
		data = data[:max(room, 0)]
	}

	b.buf.Write(data)

	return written, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
