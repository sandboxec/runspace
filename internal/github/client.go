package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// apiVersion is sent with every documented request (X-GitHub-Api-Version).
const apiVersion = "2026-03-10"

// maxErrorBody bounds how much of an error response is read for its message.
const maxErrorBody = 64 << 10

var (
	errInvalidRepo          = errors.New("invalid repository")
	errInvalidCodespaceName = errors.New("invalid codespace name")
)

// Codespace is the part of the REST codespace object that runspace uses.
// IdleTimeoutMinutes and RetentionPeriodMinutes are read only by the live
// checks, which confirm that create's settings took effect.
//
//nolint:tagliatelle // Field names are the GitHub REST API's.
type Codespace struct {
	Name                   string     `json:"name"`
	DisplayName            string     `json:"display_name"`
	State                  string     `json:"state"`
	Repository             Repository `json:"repository"`
	Machine                *Machine   `json:"machine"`
	CreatedAt              time.Time  `json:"created_at"`
	IdleTimeoutMinutes     *int       `json:"idle_timeout_minutes"`
	RetentionPeriodMinutes *int       `json:"retention_period_minutes"`
	RetentionExpiresAt     *time.Time `json:"retention_expires_at"`
}

// Repository identifies a codespace's source repository.
//
//nolint:tagliatelle // Field names are the GitHub REST API's.
type Repository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

// Machine is a codespace's machine type.
type Machine struct {
	Name string `json:"name"`
}

// CreateRequest is the body of POST /repos/{owner}/{repo}/codespaces. Fields
// without omitempty are always sent, so a zero retention is never dropped.
//
//nolint:tagliatelle // Field names are the GitHub REST API's.
type CreateRequest struct {
	Ref                        string `json:"ref,omitempty"`
	Machine                    string `json:"machine,omitempty"`
	DevcontainerPath           string `json:"devcontainer_path,omitempty"`
	DisplayName                string `json:"display_name"`
	IdleTimeoutMinutes         int    `json:"idle_timeout_minutes"`
	RetentionPeriodMinutes     int    `json:"retention_period_minutes"`
	MultiRepoPermissionsOptOut bool   `json:"multi_repo_permissions_opt_out"`
}

// StatusError is an unexpected HTTP status from the API.
type StatusError struct {
	StatusCode int
	// Message is the API's explanation, when the body carried one.
	Message string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("GitHub API returned %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		msg += ": " + e.Message
	}

	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		msg += " (" + TokenHint + ")"
	}

	return msg
}

// Client calls the documented Codespaces endpoints.
type Client struct {
	http *http.Client
	base string
}

// NewClient returns a Client that sends requests through hc to baseURL. hc
// supplies authentication; see NewClients.
func NewClient(hc *http.Client, baseURL string) *Client {
	return &Client{http: hc, base: baseURL}
}

// Create creates a codespace in repo ("owner/name"). GitHub answers 201 when
// the codespace exists and 202 when it is retrying in the background; both
// return the codespace.
func (c *Client) Create(ctx context.Context, repo string, req CreateRequest) (*Codespace, error) {
	if !ValidRepo(repo) {
		return nil, fmt.Errorf("%w %q: want OWNER/REPO", errInvalidRepo, repo)
	}

	owner, name, _ := strings.Cut(repo, "/")

	endpoint, err := url.JoinPath(c.base, "repos", owner, name, "codespaces")
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	var codespace Codespace

	_, err = c.do(ctx, http.MethodPost, endpoint, req, &codespace, http.StatusCreated, http.StatusAccepted)
	if err != nil {
		return nil, fmt.Errorf("create codespace in %s: %w", repo, err)
	}

	return &codespace, nil
}

// Get returns the named codespace of the authenticated user.
func (c *Client) Get(ctx context.Context, name string) (*Codespace, error) {
	endpoint, err := c.codespaceURL(name)
	if err != nil {
		return nil, err
	}

	var codespace Codespace

	_, err = c.do(ctx, http.MethodGet, endpoint, nil, &codespace, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("get codespace %s: %w", name, err)
	}

	return &codespace, nil
}

// List returns every codespace of the authenticated user, following
// pagination.
func (c *Client) List(ctx context.Context) ([]Codespace, error) {
	pageURL, err := url.JoinPath(c.base, "user", "codespaces")
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	pageURL += "?per_page=100"

	var all []Codespace

	for pageURL != "" {
		var page struct {
			Codespaces []Codespace `json:"codespaces"`
		}

		header, err := c.do(ctx, http.MethodGet, pageURL, nil, &page, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("list codespaces: %w", err)
		}

		all = append(all, page.Codespaces...)
		pageURL = nextLink(header.Get("Link"))
	}

	return all, nil
}

// Stop stops the named codespace.
func (c *Client) Stop(ctx context.Context, name string) error {
	endpoint, err := c.codespaceURL(name)
	if err != nil {
		return err
	}

	_, err = c.do(ctx, http.MethodPost, endpoint+"/stop", nil, nil, http.StatusOK)
	if err != nil {
		return fmt.Errorf("stop codespace %s: %w", name, err)
	}

	return nil
}

// Delete deletes the named codespace. GitHub completes the deletion in the
// background.
func (c *Client) Delete(ctx context.Context, name string) error {
	endpoint, err := c.codespaceURL(name)
	if err != nil {
		return err
	}

	_, err = c.do(ctx, http.MethodDelete, endpoint, nil, nil, http.StatusAccepted)
	if err != nil {
		return fmt.Errorf("delete codespace %s: %w", name, err)
	}

	return nil
}

func (c *Client) codespaceURL(name string) (string, error) {
	if !ValidCodespaceName(name) {
		return "", fmt.Errorf("%w %q: want letters, digits, and hyphens", errInvalidCodespaceName, name)
	}

	endpoint, err := url.JoinPath(c.base, "user", "codespaces", name)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return endpoint, nil
}

// do sends one documented request. It encodes payload as the JSON body when
// payload is non-nil, decodes the response into result when result is non-nil,
// and returns a *StatusError for any status not in want.
func (c *Client) do(
	ctx context.Context, method, endpoint string, payload, result any, want ...int,
) (http.Header, error) {
	var body io.Reader

	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}

		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", apiVersion)

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if !slices.Contains(want, resp.StatusCode) {
		return nil, statusError(resp)
	}

	if result != nil {
		err = json.NewDecoder(resp.Body).Decode(result)
		if err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.Header, nil
}

// statusError builds a *StatusError from the API's error body, which carries
// a message and, for validation failures, a list of errors.
func statusError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	// The body is diagnostic only; a malformed one leaves Message empty.
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)

	parts := []string{body.Message}

	for _, e := range body.Errors {
		switch {
		case e.Message != "":
			parts = append(parts, e.Message)
		case e.Field != "":
			parts = append(parts, e.Field+" "+e.Code)
		}
	}

	return &StatusError{
		StatusCode: resp.StatusCode,
		Message:    strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "; "),
	}
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(header string) string {
	for link := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(link, ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}

		return strings.Trim(strings.TrimSpace(target), "<>")
	}

	return ""
}

// ValidRepo reports whether repo is OWNER/REPO with each part safe as one
// URL path segment.
func ValidRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")

	return ok && validSegment(owner) && validSegment(name)
}

// ValidCodespaceName reports whether name can be a codespace name: letters,
// digits, and hyphens. Names never contain ':', which cp relies on.
func ValidCodespaceName(name string) bool {
	return name != "" && allRunes(name, func(r rune) bool { return isASCIIAlphanumeric(r) || r == '-' })
}

// validSegment reports whether s is safe as one URL path segment. url.JoinPath
// cleans "." and "..", which would address a different resource.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}

	return allRunes(s, func(r rune) bool { return isASCIIAlphanumeric(r) || r == '-' || r == '_' || r == '.' })
}

func isASCIIAlphanumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// allRunes reports whether every rune of s satisfies ok.
func allRunes(s string, ok func(rune) bool) bool {
	for _, r := range s {
		if !ok(r) {
			return false
		}
	}

	return true
}
