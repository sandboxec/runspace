// Tests for the runspace edits (see UPSTREAM.md). Upstream has no counterpart.

//nolint:depguard // depguard has no allow-list without a config file, and this test must import the package under test.
package rpc_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.sandbox.ec/runspace/internal/csconn/rpc"
)

const keyFileContents = "ssh-ed25519 FROMFILE"

// writeKeyFile writes a public key file, with the trailing newline ssh-keygen
// leaves, and returns its path.
func writeKeyFile(t *testing.T) string {
	t.Helper()

	keyFile := filepath.Join(t.TempDir(), "id.pub")

	err := os.WriteFile(keyFile, []byte(keyFileContents+"\n"), 0o600)
	if err != nil {
		t.Fatalf("writing key file: %v", err)
	}

	return keyFile
}

// The tests below bind the fixed address 127.0.0.1:16634 through
// rpc.StartSSHServerRequestKey, like invoker_test.go, so they cannot run in
// parallel.

//nolint:paralleltest // binds the fixed address 127.0.0.1:16634 shared with invoker_test.go
func TestStartSSHServerUserPublicKey(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest runspace"

	got := rpc.StartSSHServerRequestKey(t, rpc.StartSSHServerOptions{UserPublicKey: key, UserPublicKeyFile: ""})
	if got != key {
		t.Fatalf("UserPublicKey sent = %q, want %q", got, key)
	}
}

//nolint:paralleltest // binds the fixed address 127.0.0.1:16634 shared with invoker_test.go
func TestStartSSHServerUserPublicKeyWinsOverFile(t *testing.T) {
	got := rpc.StartSSHServerRequestKey(t, rpc.StartSSHServerOptions{
		UserPublicKey:     "ssh-ed25519 INMEMORY",
		UserPublicKeyFile: writeKeyFile(t),
	})
	if got != "ssh-ed25519 INMEMORY" {
		t.Fatalf("UserPublicKey sent = %q, want the in-memory key", got)
	}
}

//nolint:paralleltest // binds the fixed address 127.0.0.1:16634 shared with invoker_test.go
func TestStartSSHServerUserPublicKeyFileFallback(t *testing.T) {
	got := rpc.StartSSHServerRequestKey(t, rpc.StartSSHServerOptions{
		UserPublicKey:     "",
		UserPublicKeyFile: writeKeyFile(t),
	})
	if got != keyFileContents {
		t.Fatalf("UserPublicKey sent = %q, want the file contents trimmed", got)
	}
}

//nolint:paralleltest // binds the fixed address 127.0.0.1:16634 shared with invoker_test.go
func TestStartSSHServerNoKey(t *testing.T) {
	got := rpc.StartSSHServerRequestKey(t, rpc.StartSSHServerOptions{UserPublicKey: "", UserPublicKeyFile: ""})
	if got != "" {
		t.Fatalf("UserPublicKey sent = %q, want empty", got)
	}
}
