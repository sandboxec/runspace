//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"golang.org/x/crypto/ssh"

	"go.sandbox.ec/runspace/internal/csconn"
	"go.sandbox.ec/runspace/internal/csconn/api"
	"go.sandbox.ec/runspace/internal/csconn/connection"
	"go.sandbox.ec/runspace/internal/csconn/portforwarder"
	"go.sandbox.ec/runspace/internal/csconn/rpc"
	"go.sandbox.ec/runspace/internal/github"
)

// DialConfig says which codespace to dial and with which HTTP clients.
type DialConfig struct {
	// Name is the codespace name.
	Name string
	// APIClient carries GitHub credentials, for github.DefaultAPIURL only.
	APIClient *http.Client
	// TunnelClient is for Dev Tunnels and carries no GitHub credentials.
	TunnelClient *http.Client
	// Progress, when non-nil, receives a line for each slow phase, such as
	// waiting for a stopped codespace to start.
	Progress func(string)
	// Logger, when non-nil, receives a debug line for each step.
	Logger *log.Logger
}

var (
	// ErrTokenRejected marks a connection query that GitHub refused with 401
	// or 403.
	ErrTokenRejected = errors.New("the GitHub token was rejected")

	// errNoSSHServer is added to failures to start the codespace's SSH
	// server.
	errNoSSHServer = errors.New(
		"if the dev container has no SSH server, " +
			"add the ghcr.io/devcontainers/features/sshd:1 feature to its devcontainer.json",
	)
)

// Dial opens one connection to a codespace, starting it first if it is
// stopped. Close undoes each step in reverse order.
func Dial(ctx context.Context, cfg DialConfig) (*Conn, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard)
	}

	logger = logger.With("name", cfg.Name)

	// Connection query, then the tunnel.
	logger.Debug("Opening the tunnel")

	tunnel, err := openTunnel(ctx, cfg)
	if err != nil {
		return nil, err
	}

	forwarder, err := portforwarder.NewPortForwarder(ctx, tunnel)
	if err != nil {
		_ = tunnel.Close()

		return nil, fmt.Errorf("create port forwarder: %w", err)
	}

	// Agent RPC, with the "connected" notify and the heartbeat.
	logger.Debug("Connecting to the codespace agent")

	invoker, err := rpc.CreateInvoker(ctx, forwarder)
	if err != nil {
		return nil, abort([]func() error{forwarder.Close}, fmt.Errorf("connect to codespace agent: %w", err))
	}

	// From here on, a failure undoes these steps in reverse order, and each
	// later step adds its own undo.
	teardown := []func() error{forwarder.Close, invoker.Close}

	// SSH server, authorized for a key that exists only in memory.
	key, err := newKey()
	if err != nil {
		return nil, abort(teardown, err)
	}

	logger.Debug("Starting the SSH server")

	port, user, err := startSSHServer(ctx, invoker, key.authorized)
	if err != nil {
		return nil, abort(teardown, err)
	}

	logger.Debug("Connecting over SSH", "port", port, "user", user)

	return connectSSH(ctx, forwarder, port, user, key.signer, teardown)
}

// abort runs teardown and returns err.
func abort(teardown []func() error, err error) error {
	_ = runTeardown(teardown)

	return err
}

// openTunnel queries the codespace's connection details and opens the tunnel
// to it, starting the codespace if it is stopped. No GitHub credentials are
// involved after the query.
func openTunnel(ctx context.Context, cfg DialConfig) (*connection.CodespaceConnection, error) {
	// Connection query: GET /user/codespaces/{name}?internal=true&refresh=true.
	client := api.New(cfg.APIClient, cfg.TunnelClient, github.DefaultAPIURL)

	codespace, err := client.GetCodespace(ctx, cfg.Name, true)
	if err != nil {
		return nil, queryError(err)
	}

	// Tunnel, once the codespace is ready.
	tunnel, err := csconn.GetCodespaceConnection(ctx, progress(cfg.Progress), client, codespace)
	if err != nil {
		return nil, fmt.Errorf("connect to codespace %s: %w", cfg.Name, err)
	}

	return tunnel, nil
}

// queryError describes a failed connection query, marking a refused token.
func queryError(err error) error {
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("get codespace connection: %w: %w", ErrTokenRejected, err)
	}

	return fmt.Errorf("get codespace connection: %w", err)
}

// startSSHServer asks the codespace agent to start sshd for the authorized
// key and returns its port and the user to log in as.
func startSSHServer(ctx context.Context, invoker rpc.Invoker, authorizedKey string) (int, string, error) {
	options := rpc.StartSSHServerOptions{UserPublicKeyFile: "", UserPublicKey: authorizedKey}

	port, user, err := invoker.StartSSHServerWithOptions(ctx, options)
	if err != nil {
		// The agent explains a missing sshd in several lines of its own;
		// keep the first, so runspace prints one line.
		first, _, _ := strings.Cut(err.Error(), "\n")

		return 0, "", fmt.Errorf("%s; %w", strings.TrimRight(first, ": "), errNoSSHServer)
	}

	return port, user, nil
}

// connectSSH forwards the SSH port into one end of a pipe and runs the SSH
// handshake over the other end. It takes ownership of teardown, which runs
// on failure or when the returned Conn closes.
func connectSSH(
	ctx context.Context,
	forwarder portforwarder.PortForwarder,
	port int,
	user string,
	signer ssh.Signer,
	teardown []func() error,
) (*Conn, error) {
	opts := portforwarder.ForwardPortOpts{Port: port, Internal: true, KeepAlive: true, Visibility: ""}

	err := forwarder.ForwardPort(ctx, opts)
	if err != nil {
		return nil, abort(teardown, fmt.Errorf("forward SSH port: %w", err))
	}

	// ConnectToForwardedPort does not return on EOF, so the teardown cancels
	// its context. Its context is detached from ctx, which only bounds the
	// dial.
	local, remote := net.Pipe()
	fwdCtx, cancelFwd := context.WithCancel(context.WithoutCancel(ctx))

	teardown = append(teardown, func() error {
		cancelFwd()

		return nil
	}, remote.Close)

	forwardErr := make(chan error, 1)

	go func() {
		forwardErr <- forwarder.ConnectToForwardedPort(fwdCtx, remote, opts)
		// Unblock the SSH client if the forward ends first.
		_ = remote.Close()
	}()

	// newConn owns local and the teardown from here on, including on failure.
	conn, err := newConn(ctx, local, user, signer, defaultTiming(), teardown...)
	if err != nil {
		// A forward that ended first explains the failure better than the
		// closed pipe the handshake saw.
		select {
		case forwardFailure := <-forwardErr:
			if forwardFailure != nil {
				err = fmt.Errorf("%w (port forward: %w)", err, forwardFailure)
			}
		default:
		}

		return nil, err
	}

	return conn, nil
}

// clientKey is a fresh SSH identity: the signer and its public key as an
// authorized_keys line.
type clientKey struct {
	signer     ssh.Signer
	authorized string
}

// newKey returns a fresh ed25519 client key. The private key never leaves
// memory.
func newKey() (clientKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return clientKey{signer: nil, authorized: ""}, fmt.Errorf("generate SSH key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return clientKey{signer: nil, authorized: ""}, fmt.Errorf("generate SSH key: %w", err)
	}

	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))

	return clientKey{signer: signer, authorized: line}, nil
}

// progress adapts a line callback to csconn's progress indicator.
type progress func(string)

func (p progress) StartProgressIndicatorWithLabel(label string) {
	if p != nil {
		p(label)
	}
}

func (p progress) StopProgressIndicator() {}
