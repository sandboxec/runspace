//go:build live

package live

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSession starts runspace mcp with a minimal environment and connects.
func mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(bin, "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "RUNSPACE_TOKEN=" + os.Getenv("RUNSPACE_TOKEN")}
	cmd.Stderr = os.Stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "live"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		t.Fatalf("%s: tool error: %s", name, b.String())
	}
	if out != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

// An MCP create with idle 5m and retention 1h shows 5 and 60; exec latency
// and sshd's MaxSessions are recorded; after 10 idle minutes the connection
// closes, the codespace stops within its idle timeout, and the next call
// starts it again. -short stops before the idle wait.
func TestMCPSession(t *testing.T) {
	ownSandbox(t)
	cs := mcpSession(t)
	var created struct{ Name string }
	start := time.Now()
	callTool(t, cs, "create", map[string]any{"repo": liveRepo(), "idle": "5m", "retention": "1h"}, &created)
	t.Logf("MCP create took %v", time.Since(start).Round(time.Millisecond))
	name := created.Name
	defer callTool(t, cs, "delete", map[string]any{"names": []string{name}}, nil)

	got, err := api(t).Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if deref(got.IdleTimeoutMinutes) != 5 || deref(got.RetentionPeriodMinutes) != 60 {
		t.Errorf("idle %v, retention %v; want 5 and 60", deref(got.IdleTimeoutMinutes), deref(got.RetentionPeriodMinutes))
	}

	var times []string
	for i := range 21 {
		start := time.Now()
		var out struct {
			ExitCode int `json:"exit_code"`
		}
		callTool(t, cs, "exec", map[string]any{"name": name, "argv": []string{"true"}}, &out)
		times = append(times, time.Since(start).Round(time.Millisecond).String())
		if out.ExitCode != 0 {
			t.Errorf("exec %d: exit %d", i, out.ExitCode)
		}
	}
	t.Logf("exec latency: first %s, next twenty %s", times[0], strings.Join(times[1:], " "))
	var sshd struct{ Stdout string }
	callTool(t, cs, "exec", map[string]any{"name": name, "argv": []string{"sh", "-c", "sudo /usr/sbin/sshd -T 2>/dev/null | grep -i '^maxsessions'"}}, &sshd)
	t.Logf("sshd: %s", strings.TrimSpace(sshd.Stdout))

	if testing.Short() {
		return
	}
	idleFrom := time.Now()
	for time.Since(idleFrom) < 30*time.Minute {
		time.Sleep(30 * time.Second)
		cs, err := api(t).Get(context.Background(), name)
		if err == nil && cs.State == "Shutdown" {
			t.Logf("stopped %v after the last call", time.Since(idleFrom).Round(time.Second))
			break
		}
	}
	var out struct {
		ExitCode int `json:"exit_code"`
		Stdout   string
	}
	start = time.Now()
	callTool(t, cs, "exec", map[string]any{"name": name, "argv": []string{"echo", "awake"}}, &out)
	t.Logf("exec on the stopped sandbox took %v", time.Since(start).Round(time.Second))
	if out.ExitCode != 0 || out.Stdout != "awake\n" {
		t.Errorf("exec after stop: %+v", out)
	}
}
