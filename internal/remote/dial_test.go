//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.sandbox.ec/runspace/internal/remote"
)

const (
	queryCS1 = "GET /user/codespaces/cs-1"
	startCS1 = "POST /user/codespaces/cs-1/start"
)

var errRefused = errors.New("refused")

// stoppedCodespace fakes the GitHub API for the stopped codespace cs-1: it
// answers the connection query, fails every other request, and records each
// request as "METHOD /path".
type stoppedCodespace struct {
	mu    sync.Mutex
	calls []string
}

func (f *stoppedCodespace) RoundTrip(req *http.Request) (*http.Response, error) {
	call := req.Method + " " + req.URL.Path

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	rec := httptest.NewRecorder()
	if call == queryCS1 {
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.WriteString(`{"name":"cs-1","display_name":"laptop","state":"Shutdown",` +
			`"repository":{"full_name":"o/r"}}`)
	} else {
		rec.WriteHeader(http.StatusBadRequest)
	}

	return rec.Result(), nil
}

func (f *stoppedCodespace) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// dialFake dials the named codespace through stoppedCodespace fakes with
// admit. It returns the requests made to the GitHub API and to Dev Tunnels,
// and the dial's error.
func dialFake(t *testing.T, name string, admit func(remote.Codespace) error) ([]string, []string, error) {
	t.Helper()

	githubAPI, tunnels := new(stoppedCodespace), new(stoppedCodespace)
	apiClient, tunnelClient := new(http.Client), new(http.Client)
	apiClient.Transport, tunnelClient.Transport = githubAPI, tunnels

	conn, err := remote.Dial(context.Background(), remote.DialConfig{
		Name:         name,
		APIClient:    apiClient,
		TunnelClient: tunnelClient,
		Progress:     nil,
		Logger:       nil,
		Admit:        admit,
	})
	if conn != nil {
		_ = conn.Close()

		t.Fatal("dialed the fake codespace")
	}

	return githubAPI.Calls(), tunnels.Calls(), err
}

// Admit sees what the connection query found before Dial acts on the
// codespace: a refusal ends the dial before it starts the stopped codespace.
func TestDialAdmitsBeforeActing(t *testing.T) {
	t.Parallel()

	var seen remote.Codespace

	calls, tunnelCalls, err := dialFake(t, "cs-1", func(codespace remote.Codespace) error {
		seen = codespace

		return errRefused
	})
	if !errors.Is(err, errRefused) {
		t.Errorf("Dial = %v, want the refusal", err)
	}

	if want := (remote.Codespace{DisplayName: "laptop", RepoName: "r"}); seen != want {
		t.Errorf("Admit saw %+v, want %+v", seen, want)
	}

	if !slices.Equal(calls, []string{queryCS1}) || len(tunnelCalls) != 0 {
		t.Errorf("a refused dial requested %v and %v; want only the connection query", calls, tunnelCalls)
	}

	// The control: an admitted dial goes on to start the codespace.
	calls, _, _ = dialFake(t, "cs-1", func(remote.Codespace) error { return nil })
	if !slices.Contains(calls, startCS1) {
		t.Errorf("an admitted dial requested %v; want %s among them", calls, startCS1)
	}
}

// A failed connection query names the codespace, as it is the only lookup
// of a codespace that a command makes.
func TestDialQueryErrorNamesTheCodespace(t *testing.T) {
	t.Parallel()

	_, _, err := dialFake(t, "cs-2", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "get codespace cs-2: ") {
		t.Errorf("Dial = %v, want it to start with %q", err, "get codespace cs-2: ")
	}
}
