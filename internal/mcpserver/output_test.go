//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package mcpserver_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"go.sandbox.ec/runspace/internal/mcpserver"
	"go.sandbox.ec/runspace/internal/sandbox"
)

// writeChunks writes each chunk to writer, failing the test on an error.
func writeChunks(t *testing.T, writer io.Writer, chunks ...string) {
	t.Helper()

	for _, chunk := range chunks {
		_, err := writer.Write([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		writes    []string
		want      string
		truncated bool
	}{
		{[]string{"a", "b"}, "ab", false},
		{[]string{strings.Repeat("x", mcpserver.OutputCap)}, strings.Repeat("x", mcpserver.OutputCap), false},
		{
			[]string{"HEAD", strings.Repeat("x", mcpserver.OutputCap)},
			"[truncated 4 bytes]\n" + strings.Repeat("x", mcpserver.OutputCap),
			true,
		},
		{
			[]string{strings.Repeat("y", mcpserver.OutputCap+10)},
			"[truncated 10 bytes]\n" + strings.Repeat("y", mcpserver.OutputCap),
			true,
		},
		// A rune cut by the drop is skipped and counted.
		{
			[]string{"é" + strings.Repeat("z", mcpserver.OutputCap-1)},
			"[truncated 2 bytes]\n" + strings.Repeat("z", mcpserver.OutputCap-1),
			true,
		},
	}
	for index, test := range tests {
		tail := mcpserver.NewTail()
		writeChunks(t, tail, test.writes...)

		got := tail.String()
		if got != test.want || tail.Truncated() != test.truncated {
			t.Errorf("%d: got %d bytes %q..., truncated %v", index, len(got), got[:min(len(got), 30)], tail.Truncated())
		}
	}
}

// Many small writes wrap the ring correctly.
func TestTailWrapsRing(t *testing.T) {
	t.Parallel()

	tail := mcpserver.NewTail()

	var written strings.Builder

	for index := range 20000 {
		chunk := strings.Repeat(string(rune('a'+index%26)), index%7+1)
		written.WriteString(chunk)
		writeChunks(t, tail, chunk)
	}

	all := written.String()
	if got := tail.String(); !strings.HasSuffix(got, all[len(all)-mcpserver.OutputCap:]) {
		t.Error("wrapped ring lost the tail")
	}
}

// BenchmarkTail measures copying command output into the capped tail, the
// MCP hot path for chatty commands.
func BenchmarkTail(b *testing.B) {
	chunk := make([]byte, mcpserver.OutputCap)
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()

	tail := mcpserver.NewTail()
	for b.Loop() {
		_, err := tail.Write(chunk)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The descriptions state the cap in KiB; keep them in step with the cap.
func TestDescriptionsStateOutputCap(t *testing.T) {
	t.Parallel()

	want := fmt.Sprintf("%d KiB", mcpserver.OutputCap>>10)
	descriptions := map[string]string{"exec": mcpserver.ExecDescription, "diff": mcpserver.DiffDescription}

	for name, description := range descriptions {
		if !strings.Contains(description, want) {
			t.Errorf("%s description does not state %s", name, want)
		}
	}
}

// The descriptions of diff and cp state the default timeout; keep them in
// step with it.
func TestDescriptionsStateDefaultTimeout(t *testing.T) {
	t.Parallel()

	want := "timeout (default " + sandbox.FormatDuration(sandbox.DefaultTimeout) + ")"
	descriptions := map[string]string{"diff": mcpserver.DiffDescription, "cp": mcpserver.CpDescription}

	for name, description := range descriptions {
		if !strings.Contains(description, want) {
			t.Errorf("%s description does not state %q", name, want)
		}
	}
}
