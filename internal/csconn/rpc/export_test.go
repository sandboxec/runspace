// Test hooks for runspace's own tests (see UPSTREAM.md). Upstream has no
// counterpart. It exposes the mock server in invoker_test.go to the external
// test package through one helper.

//nolint:depguard // depguard has no allow-list without a config file, and this hook imports the generated ssh package.
package rpc

import (
	"context"
	"testing"

	"go.sandbox.ec/runspace/internal/csconn/rpc/ssh"
)

const (
	mockServerPort = "1234"
	mockServerUser = "test"
)

// StartSSHServerRequestKey starts the SSH server through the mock codespace
// host, which listens on the fixed address 127.0.0.1:16634 like the tests in
// invoker_test.go, so callers must not run in parallel. It requires exactly one
// StartRemoteServerAsync call and returns the UserPublicKey that call carried.
func StartSSHServerRequestKey(t *testing.T, options StartSSHServerOptions) string {
	t.Helper()

	var (
		gotKey string
		calls  int
	)

	server := newMockServer()
	server.StartRemoteServerAsyncFunc = func(
		_ context.Context,
		req *ssh.StartRemoteServerRequest,
	) (*ssh.StartRemoteServerResponse, error) {
		calls++
		gotKey = req.GetUserPublicKey()

		return &ssh.StartRemoteServerResponse{
			ServerPort: mockServerPort,
			User:       mockServerUser,
			Message:    "",
			Result:     true,
		}, nil
	}

	invoker, stop, err := createTestInvoker(t, server)
	if err != nil {
		t.Fatalf("error connecting to internal server: %v", err)
	}
	defer stop()

	_, _, err = invoker.StartSSHServerWithOptions(context.Background(), options)
	if err != nil {
		t.Fatalf("StartSSHServerWithOptions returned an error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected 1 StartRemoteServerAsync call, got %d", calls)
	}

	return gotKey
}
