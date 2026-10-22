//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	csapi "go.sandbox.ec/runspace/internal/csconn/api"
	"go.sandbox.ec/runspace/internal/github"
)

// Only RUNSPACE_TOKEN is read.
func TestTokenReadsOnlyRunspaceToken(t *testing.T) {
	t.Parallel()

	env := map[string]string{"GITHUB_TOKEN": "ambient", "GH_TOKEN": "ambient"}

	var read []string

	getenv := func(key string) string {
		read = append(read, key)

		return env[key]
	}

	_, err := github.Token("", getenv)
	if err == nil || !strings.Contains(err.Error(), github.TokenEnv) || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("Token() error = %v, want one naming --token and %s", err, github.TokenEnv)
	}

	env[github.TokenEnv] = "tok"

	got, err := github.Token("", getenv)
	if err != nil || got != "tok" {
		t.Fatalf("Token() = %q, %v; want tok", got, err)
	}

	for _, key := range read {
		if key != github.TokenEnv {
			t.Errorf("Token read %s", key)
		}
	}
}

// A given token, as from --token, wins, and then no variable is read.
func TestTokenGivenWins(t *testing.T) {
	t.Parallel()

	getenv := func(key string) string {
		t.Errorf("Token read %s", key)

		return "env"
	}

	got, err := github.Token("flag", getenv)
	if err != nil || got != "flag" {
		t.Fatalf("Token() = %q, %v; want flag", got, err)
	}
}

// Every documented call sends the token, the documented Accept header,
// the API version, and runspace's User-Agent.
func TestDocumentedCallHeaders(t *testing.T) {
	t.Parallel()

	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		seen = append(seen, req.Method+" "+req.URL.Path)

		checkDocumentedHeaders(t, req)
		respondToCodespaceCall(t, writer, req)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	_, err := client.Create(ctx, "o/r", emptyCreateRequest())
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Get(ctx, "cs-1")
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = client.Stop(ctx, "cs-1")
	if err != nil {
		t.Fatal(err)
	}

	err = client.Delete(ctx, "cs-1")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"POST /repos/o/r/codespaces",
		"GET /user/codespaces/cs-1",
		"GET /user/codespaces",
		"POST /user/codespaces/cs-1/stop",
		"DELETE /user/codespaces/cs-1",
	}
	if !slices.Equal(seen, want) {
		t.Errorf("requests = %q, want %q", seen, want)
	}
}

func checkDocumentedHeaders(t *testing.T, req *http.Request) {
	t.Helper()

	want := map[string]string{
		"Authorization":        "Bearer " + testToken,
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": github.APIVersion,
		"User-Agent":           "runspace",
	}
	for key, value := range want {
		if got := req.Header.Get(key); got != value {
			t.Errorf("%s %s: %s = %q, want %q", req.Method, req.URL.Path, key, got, value)
		}
	}
}

func respondToCodespaceCall(t *testing.T, writer http.ResponseWriter, req *http.Request) {
	t.Helper()

	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/stop"):
		respond(t, writer, `{}`)
	case req.Method == http.MethodPost:
		writer.WriteHeader(http.StatusCreated)
		respond(t, writer, `{"name":"cs-1"}`)
	case req.Method == http.MethodDelete:
		writer.WriteHeader(http.StatusAccepted)
	case req.URL.Path == "/user/codespaces":
		respond(t, writer, `{"codespaces":[]}`)
	default:
		respond(t, writer, `{"name":"cs-1"}`)
	}
}

// The Dev Tunnels client never sends Authorization, and the API client
// sends it only to the API host, including across a redirect.
func TestCredentialsStayOnAPIHost(t *testing.T) {
	t.Parallel()

	other := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("%s %s on another host: Authorization = %q", req.Method, req.URL, got)
		}

		respond(t, writer, `{}`)
	}))
	defer other.Close()

	apiSrv := httptest.NewServer(http.RedirectHandler(other.URL+"/elsewhere", http.StatusFound))
	defer apiSrv.Close()

	api, tunnels, err := github.NewClients(testToken, apiSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if api == tunnels || github.AuthBaseTransport(api) == tunnels.Transport {
		t.Fatal("API and tunnels clients share state")
	}

	for name, client := range map[string]*http.Client{"tunnels": tunnels, "api": api} {
		err = getAndClose(t, client, other.URL+"/direct")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The API host redirects to the other host.
	err = getAndClose(t, api, apiSrv.URL+"/redirect")
	if err != nil {
		t.Fatal(err)
	}
}

func TestListFollowsPagination(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", req.URL.Query().Get("per_page"))
		}

		page := req.URL.Query().Get("page")
		if page == "" {
			nextPage := srv.URL + "/user/codespaces?per_page=100&page=2"
			writer.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%[1]s>; rel="last"`, nextPage))
			respond(t, writer, `{"codespaces":[{"name":"a"},{"name":"b"}]}`)

			return
		}

		respond(t, writer, `{"codespaces":[{"name":"c"}]}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(got))
	for _, cs := range got {
		names = append(names, cs.Name)
	}

	if want := []string{"a", "b", "c"}; !slices.Equal(names, want) {
		t.Errorf("names = %q, want %q", names, want)
	}
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		body   string
		want   []string
	}{
		{
			http.StatusUnauthorized, `{"message":"Bad credentials"}`,
			[]string{"401", "Bad credentials", github.TokenEnv, "codespace scope"},
		},
		{http.StatusForbidden, `{"message":"Must have admin rights"}`, []string{"403", github.TokenEnv}},
		{
			http.StatusUnprocessableEntity,
			`{"message":"Validation Failed","errors":[` +
				`{"message":"idle timeout must be between 5 and 240"},{"field":"machine","code":"invalid"}]}`,
			[]string{"422", "Validation Failed; idle timeout must be between 5 and 240; machine invalid"},
		},
		{http.StatusInternalServerError, `not json`, []string{"500 Internal Server Error"}},
	}
	for _, test := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(test.status)
			respond(t, writer, test.body)
		}))
		_, err := newTestClient(t, srv.URL).Get(context.Background(), "cs-1")

		srv.Close()

		if err == nil {
			t.Fatalf("status %d: no error", test.status)
		}

		for _, want := range test.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("status %d: error %q does not contain %q", test.status, err, want)
			}
		}

		if strings.Contains(err.Error(), testToken) {
			t.Errorf("status %d: error %q leaks the token", test.status, err)
		}

		blamesToken := strings.Contains(err.Error(), github.TokenEnv)
		if test.status != http.StatusUnauthorized && test.status != http.StatusForbidden && blamesToken {
			t.Errorf("status %d: error %q blames the token", test.status, err)
		}
	}
}

func TestCreateSendsZeroRetention(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		var body map[string]any

		err := json.NewDecoder(req.Body).Decode(&body)
		if err != nil {
			t.Error(err)

			return
		}

		if value, ok := body["retention_period_minutes"]; !ok || value != 0.0 {
			t.Errorf("retention_period_minutes = %v (present %v), want 0", value, ok)
		}

		if _, ok := body["devcontainer_path"]; ok {
			t.Error("devcontainer_path sent while empty")
		}

		writer.WriteHeader(http.StatusAccepted)
		respond(t, writer, `{"name":"cs-1"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Create(context.Background(), "o/r", emptyCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		t.Errorf("request sent: %s %s", req.Method, req.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	ctx := context.Background()
	for _, name := range []string{"", ".", "..", "a/b", "a?b"} {
		_, err := client.Get(ctx, name)
		if err == nil {
			t.Errorf("Get(%q) succeeded", name)
		}
	}

	for _, repo := range []string{"o", "o/..", "../r", "o/r/x"} {
		_, err := client.Create(ctx, repo, emptyCreateRequest())
		if err == nil {
			t.Errorf("Create(%q) succeeded", repo)
		}
	}
}

// testToken is the test clients' token, distinct enough that finding it in
// an error means the error leaks it.
const testToken = "c0ffee-test-secret"

func newTestClient(t *testing.T, base string) *github.Client {
	t.Helper()

	api, _, err := github.NewClients(testToken, base)
	if err != nil {
		t.Fatal(err)
	}

	return github.NewClient(api, base)
}

// emptyCreateRequest is a CreateRequest with every field left at its zero
// value.
func emptyCreateRequest() github.CreateRequest {
	return github.CreateRequest{
		Ref:                        "",
		Machine:                    "",
		DevcontainerPath:           "",
		DisplayName:                "",
		IdleTimeoutMinutes:         0,
		RetentionPeriodMinutes:     0,
		MultiRepoPermissionsOptOut: false,
	}
}

// respond writes body to writer, failing the test if the write fails.
func respond(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()

	_, err := io.WriteString(writer, body)
	if err != nil {
		t.Error(err)
	}
}

// getAndClose issues a GET with client and closes the response body.
func getAndClose(t *testing.T, client *http.Client, target string) error {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	err = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("close response body: %w", err)
	}

	return nil
}

// The internal connection query keeps upstream's headers (the v3 Accept
// header and no version header) and still authenticates through the API
// client from NewClients.
func TestInternalQueryHeaders(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if got := req.URL.Query().Get("internal") + req.URL.Query().Get("refresh"); got != "truetrue" {
			t.Errorf("query = %q, want internal=true&refresh=true", req.URL.RawQuery)
		}

		want := map[string]string{
			"Authorization":        "Bearer " + testToken,
			"Accept":               "application/vnd.github.v3+json",
			"X-Github-Api-Version": "",
			"User-Agent":           "runspace",
		}
		for key, value := range want {
			if got := req.Header.Get(key); got != value {
				t.Errorf("%s = %q, want %q", key, got, value)
			}
		}

		respond(t, writer, `{"name":"cs-1"}`)
	}))
	defer srv.Close()

	apiClient, tunnels, err := github.NewClients(testToken, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	_, err = csapi.New(apiClient, tunnels, srv.URL).GetCodespace(context.Background(), "cs-1", true)
	if err != nil {
		t.Fatal(err)
	}
}
