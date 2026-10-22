// Package github reads runspace's token and calls the documented GitHub
// Codespaces REST endpoints.
package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

var (
	errTokenUnset = errors.New(
		"no GitHub token; pass --token or set " + TokenEnv + " to a token with the codespace scope")
	errNotAbsolute = errors.New("must be absolute")
)

const (
	// TokenEnv is the only variable runspace reads its token from.
	TokenEnv = "RUNSPACE_TOKEN"

	// TokenHint explains a token GitHub rejected.
	TokenHint = "check that the token from --token or " + TokenEnv + " is valid, unexpired, and has the codespace scope"

	// DefaultAPIURL is the GitHub REST API base URL.
	DefaultAPIURL = "https://api.github.com"

	userAgent = "runspace"

	// apiTimeout bounds each REST request. Requests are small, and long
	// waits poll with fresh requests.
	apiTimeout = time.Minute
)

// Token returns runspace's GitHub token: given, the --token value, unless
// it is "", else RUNSPACE_TOKEN. It reads no other variable, so an ambient
// GITHUB_TOKEN or GH_TOKEN is never picked up.
func Token(given string, getenv func(string) string) (string, error) {
	if given != "" {
		return given, nil
	}

	token := getenv(TokenEnv)
	if token == "" {
		return "", errTokenUnset
	}

	return token, nil
}

// NewClients returns two HTTP clients that share nothing.
//
// The API client sends the token only to the scheme and host of baseURL,
// including on redirects. The tunnels client is for Dev Tunnels and never
// carries GitHub credentials.
func NewClients(token, baseURL string) (*http.Client, *http.Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse API URL %q: %w", baseURL, err)
	}

	if base.Scheme == "" || base.Host == "" {
		return nil, nil, fmt.Errorf("API URL %q %w", baseURL, errNotAbsolute)
	}

	api := &http.Client{
		Transport: &authTransport{
			base:   newTransport(),
			scheme: base.Scheme,
			host:   base.Host,
			token:  token,
		},
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       apiTimeout,
	}
	// No overall timeout: the Dev Tunnels SDK manages its own requests and
	// contexts.
	tunnels := &http.Client{
		Transport:     newTransport(),
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	return api, tunnels, nil
}

func newTransport() *http.Transport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Someone replaced http.DefaultTransport; keep its proxy behavior.
		transport := new(http.Transport)
		transport.Proxy = http.ProxyFromEnvironment

		return transport
	}

	return defaultTransport.Clone()
}

// authTransport adds runspace's User-Agent to every request and the token to
// requests for the API host only.
type authTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
	token  string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", userAgent)

	if req.URL.Scheme == t.scheme && req.URL.Host == t.host {
		req.Header.Set("Authorization", "Bearer "+t.token)
	} else {
		req.Header.Del("Authorization")
	}

	// http.Client type-asserts this error (tls.RecordHeaderError), and
	// url.Error.Timeout asserts its direct cause, so a wrapper would change
	// their results.
	return t.base.RoundTrip(req) //nolint:wrapcheck // net/http inspects the unwrapped error
}
