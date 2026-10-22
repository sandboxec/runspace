//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"go.sandbox.ec/runspace/internal/csconn"
	csapi "go.sandbox.ec/runspace/internal/csconn/api"
	"go.sandbox.ec/runspace/internal/csconn/portforwarder"
	rpcssh "go.sandbox.ec/runspace/internal/csconn/rpc/ssh"
	"go.sandbox.ec/runspace/internal/github"
	"go.sandbox.ec/runspace/internal/remote"
)

// TestDial dials the codespace named by RUNSPACE_LIVE_CODESPACE through the
// copied protocol code and runs commands over one Conn.
func TestDial(t *testing.T) {
	name := os.Getenv("RUNSPACE_LIVE_CODESPACE")
	if name == "" {
		t.Skip("RUNSPACE_LIVE_CODESPACE is not set")
	}
	token, err := github.Token("", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	apiClient, tunnelClient, err := github.NewClients(token, github.DefaultAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	start := time.Now()
	c, err := remote.Dial(ctx, remote.DialConfig{
		Name:         name,
		APIClient:    apiClient,
		TunnelClient: tunnelClient,
		Progress:     func(s string) { t.Log(s) },
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	t.Logf("dial took %v", time.Since(start))

	run := func(argv ...string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code, err := c.Exec(ctx, remote.Cmd{Dir: "/workspaces/" + liveRepoName(), Argv: argv, Stdout: &out, Stderr: &out})
		if err != nil {
			t.Fatalf("Exec %q: %v", argv, err)
		}
		return code, out.String()
	}

	if code, out := run("true"); code != 0 {
		t.Errorf("true: exit %d, output %q", code, out)
	}
	if code, out := run("sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit 7: exit %d, output %q", code, out)
	}
	for i := range 10 {
		start := time.Now()
		if code, out := run("true"); code != 0 {
			t.Errorf("session %d: exit %d, output %q", i, code, out)
		}
		t.Logf("session %d took %v", i, time.Since(start))
	}
	_, out := run("sh", "-c", "id -un; uname -n; ssh -V 2>&1; ls -l /usr/sbin/sshd; git -C . rev-parse HEAD")
	t.Logf("remote facts:\n%s", out)
}

// TestInProcessGRPC checks that gRPC can reach the codespace agent over an
// in-process stream instead of the 127.0.0.1 listener. It dials through
// net.Pipe and asks the agent to start sshd, as Dial does.
func TestInProcessGRPC(t *testing.T) {
	name := shared(t)
	token, _ := github.Token("", os.Getenv)
	apiClient, tunnelClient, err := github.NewClients(token, github.DefaultAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := csapi.New(apiClient, tunnelClient, github.DefaultAPIURL)
	cs, err := client.GetCodespace(ctx, name, true)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := csconn.GetCodespaceConnection(ctx, noProgress{}, client, cs)
	if err != nil {
		t.Fatal(err)
	}
	fwd, err := portforwarder.NewPortForwarder(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	opts := portforwarder.ForwardPortOpts{Port: 16634, Internal: true}
	if err := fwd.ForwardPort(ctx, opts); err != nil {
		t.Fatal(err)
	}

	pipeCtx, stopPipes := context.WithCancel(context.Background())
	defer stopPipes()
	dialer := func(context.Context, string) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() {
			fwd.ConnectToForwardedPort(pipeCtx, remote, opts)
			remote.Close()
		}()
		return local, nil
	}
	gc, err := grpc.NewClient("passthrough:///codespace-agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer))
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := xssh.NewPublicKey(pub)
	callCtx := metadata.AppendToOutgoingContext(ctx, "Authorization", "Bearer token")
	resp, err := rpcssh.NewSshServerHostClient(gc).StartRemoteServerAsync(callCtx, &rpcssh.StartRemoteServerRequest{
		UserPublicKey: strings.TrimSpace(string(xssh.MarshalAuthorizedKey(key))),
	})
	if err != nil {
		t.Fatalf("gRPC over net.Pipe failed: %v", err)
	}
	t.Logf("StartRemoteServerAsync over net.Pipe: result=%v port=%s user=%s", resp.Result, resp.ServerPort, resp.User)
	if !resp.Result {
		t.Errorf("the agent refused: %s", resp.Message)
	}
}

type noProgress struct{}

func (noProgress) StartProgressIndicatorWithLabel(string) {}
func (noProgress) StopProgressIndicator()                 {}
