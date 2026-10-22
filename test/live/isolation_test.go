//go:build live

package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The sandbox's own GITHUB_TOKEN cannot read another private repository.
// runspace's sessions don't export it, so the probe reads it from the
// codespace's own env file.
func TestSandboxTokenScope(t *testing.T) {
	name := shared(t)
	probe := func(path string) string {
		r := runspace(t, "", nil, "exec", name, "sh", "-c", sandboxToken+
			`curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $GITHUB_TOKEN" "https://api.github.com$1"`, "probe", path)
		return r.stdout
	}
	if code := probe("/repos/" + liveRepo()); code != "200" {
		t.Fatalf("the sandbox token cannot read its own repository (%s); the probe is broken", code)
	}
	t.Logf("/user/codespaces with the sandbox token: %s", probe("/user/codespaces"))
	other := os.Getenv("RUNSPACE_LIVE_PRIVATE_REPO")
	if other == "" {
		t.Log("RUNSPACE_LIVE_PRIVATE_REPO unset; skipping the other-repo read")
		return
	}
	if code := probe("/repos/" + other); code == "200" {
		t.Errorf("the sandbox token read %s", other)
	}
}

// sandboxToken loads the codespace's own GITHUB_TOKEN into a script.
const sandboxToken = "set -a; . /workspaces/.codespaces/shared/.env; set +a; "

// What one sandbox's own GITHUB_TOKEN reaches in a sibling sandbox of the
// same identity and repository. The results are recorded; only stopping the
// sibling is asserted.
func TestCrossSandbox(t *testing.T) {
	ownSandbox(t)
	a := shared(t)
	r := runspace(t, "", nil, "create", "-R", liveRepo())
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	b := strings.TrimSpace(r.stdout)
	defer runspace(t, "", nil, "delete", b)

	// A private forwarded port in b. gh registers the port while it
	// forwards it.
	// A new codespace can take more than a few seconds to start the server.
	start := `setsid nohup python3 -m http.server 8000 >/tmp/http.log 2>&1 </dev/null &
for i in $(seq 30); do code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8000/) && break; sleep 1; done
[ "$code" = 200 ] || cat /tmp/http.log >&2
printf %s "$code"`
	if r := runspace(t, "", nil, "exec", b, "sh", "-c", start); r.stdout != "200" {
		t.Fatalf("server in %s: %q %s", b, r.stdout, r.stderr)
	}
	fwdCtx, stopFwd := context.WithCancel(context.Background())
	defer stopFwd()
	fwd := exec.CommandContext(fwdCtx, "gh", "codespace", "ports", "forward", "8000:18765", "-c", b)
	if err := fwd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Second)

	url := "https://" + b + "-8000.app.github.dev/"
	port := runspace(t, "", nil, "exec", a, "sh", "-c", sandboxToken+
		`curl -s -L --max-time 25 -H "X-Github-Token: $GITHUB_TOKEN" "$1" | grep -c 'Directory listing'`, "probe", url)
	t.Logf("%s's token opens %s's private port: %v", a, b, strings.TrimSpace(port.stdout) != "0")

	creds := runspace(t, "", nil, "exec", a, "sh", "-c", sandboxToken+
		`curl -s -H "Authorization: Bearer $GITHUB_TOKEN" "https://api.github.com/user/codespaces/$1?internal=true&refresh=true" | grep -c connectAccessToken`, "probe", b)
	t.Logf("%s's token gets %s's tunnel credentials: %v", a, b, strings.TrimSpace(creds.stdout) != "0")

	stop := runspace(t, "", nil, "exec", a, "sh", "-c", sandboxToken+
		`curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $GITHUB_TOKEN" "https://api.github.com/user/codespaces/$1/stop"`, "probe", b)
	if stop.stdout == "200" || stop.stdout == "202" {
		t.Errorf("%s's token stopped %s", a, b)
	}

	// Codespaces run on x86-64.
	if r := runspace(t, "", nil, "cp", linuxBinary(t, "amd64"), a+":/tmp/runspace"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	reach := runspace(t, "", nil, "exec", a, "sh", "-c", sandboxToken+
		`chmod +x /tmp/runspace; RUNSPACE_TOKEN="$GITHUB_TOKEN" /tmp/runspace exec "$1" uname -n`, "probe", b)
	host := strings.TrimSpace(runspace(t, "", nil, "exec", b, "uname", "-n").stdout)
	t.Logf("%s ran a command in %s: %v (%q, %s is %q)", a, b, strings.TrimSpace(reach.stdout) == host, strings.TrimSpace(reach.stdout), b, host)
}

// runspace mcp running as a different UID cannot copy files out of your
// home directory.
func TestMCPAsOtherUser(t *testing.T) {
	home, _ := os.UserHomeDir()
	// asOtherUser runs args in a container as uid 2000, with your home
	// mounted. Docker runs Linux on this host's architecture, natively or in
	// a VM.
	asOtherUser := func(args ...string) *exec.Cmd {
		//nolint:gosec // G204: the check runs docker with arguments of its own.
		return exec.CommandContext(t.Context(), "docker", append([]string{"run", "--rm", "-i", "--user", "2000:2000",
			"--platform", "linux/" + runtime.GOARCH, "-v", home + ":" + home + ":ro"}, args...)...)
	}
	// uid 2000 stands in for another user only if your home keeps its owner
	// in the container. Docker Desktop shows shared files as owned by
	// whoever reads them.
	owner, _ := asOtherUser("alpine:3.24", "stat", "-c", "%u", home).Output()
	if strings.TrimSpace(string(owner)) == "2000" {
		t.Skip("Docker shows your home directory as owned by uid 2000, so uid 2000 is not another user")
	}

	cmd := asOtherUser("-v", linuxBinary(t, runtime.GOARCH)+":/usr/local/bin/runspace:ro",
		"-e", "RUNSPACE_TOKEN", "-e", "HOME=/tmp",
		"alpine:3.24", "/usr/local/bin/runspace", "mcp")
	cmd.Env = append(os.Environ(), "RUNSPACE_TOKEN="+os.Getenv("RUNSPACE_TOKEN"))
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent-user"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// The server starts before the sandbox, so a server that cannot start
	// fails the check before a codespace is paid for.
	name := shared(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cp", Arguments: map[string]any{
		"src": filepath.Join(home, ".bashrc"), "dst": name + ":stolen",
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := toolText(res)
	t.Logf("cp from %s as uid 2000: error=%v %q", home, res.IsError, text)
	if !res.IsError || !strings.Contains(text, "permission denied") {
		t.Errorf("copying from your home directory as another user was not refused")
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "exec", Arguments: map[string]any{
		"name": name, "argv": []string{"uname", "-n"},
	}})
	if err != nil || res.IsError {
		t.Fatalf("exec as uid 2000: %v %q", err, toolText(res))
	}
	t.Logf("uname -n as uid 2000: %s", toolText(res))
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
