// Tests for the runspace edits (see UPSTREAM.md). Upstream has no counterpart.

//nolint:depguard // depguard has no allow-list without a config file, and this test must import the package under test.
package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.sandbox.ec/runspace/internal/csconn/api"
)

const (
	codespaceName      = "cs-1"
	codespacePath      = "/user/codespaces/cs-1"
	startCodespacePath = "/user/codespaces/cs-1/start"
	notFoundMessage    = "Codespace not found"
)

// newTestAPI serves handler on a test server that is closed when the test
// ends and returns an API client that talks to it. suffix is appended to the
// server URL to form the API base URL.
func newTestAPI(t *testing.T, handler http.HandlerFunc, suffix string) *api.API {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return api.New(srv.Client(), nil, srv.URL+suffix)
}

func TestCodespaceNameValidation(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	client := newTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}, "")

	invalid := []string{
		"",
		"..",
		".",
		"../x",
		"a/b",
		"a b",
		"a_b",
		"a.b",
		"a%2e%2e",
		"a?b=c",
		"a#b",
		"name\n",
		"naéme",
	}
	for _, name := range invalid {
		_, err := client.GetCodespace(context.Background(), name, false)
		if err == nil {
			t.Errorf("GetCodespace(%q) succeeded, want an error", name)
		}

		err = client.StartCodespace(context.Background(), name)
		if err == nil {
			t.Errorf("StartCodespace(%q) succeeded, want an error", name)
		}
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("expected no requests for invalid names, got %d", got)
	}

	for _, name := range []string{"a", "Abc-123", "obscure-space-guacamole-7vgqx9xrx5r3rv"} {
		_, err := client.GetCodespace(context.Background(), name, false)
		if err != nil {
			t.Errorf("GetCodespace(%q) returned error: %v", name, err)
		}

		err = client.StartCodespace(context.Background(), name)
		if err != nil {
			t.Errorf("StartCodespace(%q) returned error: %v", name, err)
		}
	}

	if got := requests.Load(); got != 6 {
		t.Fatalf("expected 6 requests for valid names, got %d", got)
	}
}

func TestGetCodespaceRequest(t *testing.T) {
	t.Parallel()

	var gotPath, gotQuery, gotAccept string

	client := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAccept = r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"name":"cs-1","state":"Available"}`))
	}, "/")

	codespace, err := client.GetCodespace(context.Background(), codespaceName, true)
	if err != nil {
		t.Fatal(err)
	}

	if codespace.Name != codespaceName || codespace.State != api.CodespaceStateAvailable {
		t.Fatalf("unexpected codespace: %+v", codespace)
	}

	if gotPath != codespacePath {
		t.Errorf("path = %q", gotPath)
	}

	if gotQuery != "internal=true&refresh=true" {
		t.Errorf("query = %q", gotQuery)
	}

	if gotAccept != "application/vnd.github.v3+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
}

func TestGetCodespaceStatusError(t *testing.T) {
	t.Parallel()

	client := newTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"` + notFoundMessage + `"}`))
	}, "")

	_, err := client.GetCodespace(context.Background(), codespaceName, false)

	var statusErr *api.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected a StatusError, got %v", err)
	}

	if statusErr.StatusCode != http.StatusNotFound || statusErr.Message != notFoundMessage {
		t.Fatalf("unexpected StatusError: %+v", statusErr)
	}

	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), notFoundMessage) {
		t.Fatalf("unexpected error text: %q", err)
	}
}

func TestStatusErrorWithoutJSONBody(t *testing.T) {
	t.Parallel()

	client := newTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>nope</html>"))
	}, "")

	err := client.StartCodespace(context.Background(), codespaceName)

	var statusErr *api.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected a StatusError, got %v", err)
	}

	if statusErr.StatusCode != http.StatusForbidden || statusErr.Message != "403 Forbidden" {
		t.Fatalf("unexpected StatusError: %+v", statusErr)
	}
}

func TestStartCodespaceStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status  int
		wantErr bool
	}{
		{http.StatusOK, false},
		{http.StatusConflict, false}, // already starting
		{http.StatusNotFound, true},
		{http.StatusUnauthorized, true},
	}
	for _, testCase := range tests {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotPath string

			client := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path

				w.WriteHeader(testCase.status)
			}, "")

			err := client.StartCodespace(context.Background(), codespaceName)
			if (err != nil) != testCase.wantErr {
				t.Errorf("status %d: err = %v, wantErr %v", testCase.status, err, testCase.wantErr)
			}

			if gotMethod != http.MethodPost || gotPath != startCodespacePath {
				t.Errorf("status %d: request = %s %s", testCase.status, gotMethod, gotPath)
			}
		})
	}
}

func TestExternalHTTPClientIsSeparate(t *testing.T) {
	t.Parallel()

	githubClient := &http.Client{Transport: nil, CheckRedirect: nil, Jar: nil, Timeout: 0}
	externalClient := &http.Client{Transport: nil, CheckRedirect: nil, Jar: nil, Timeout: 0}
	client := api.New(githubClient, externalClient, "https://api.github.com")

	got, err := client.ExternalHTTPClient()
	if err != nil {
		t.Fatal(err)
	}

	if got != externalClient || got == githubClient {
		t.Fatal("ExternalHTTPClient did not return the external client")
	}
}
