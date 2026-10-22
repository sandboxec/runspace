//nolint:all // Pinned upstream copy; edits are logged in UPSTREAM.md.
package api

// For descriptions of service interfaces, see:
// - https://docs.github.com/en/rest/reference/repos (for api.github.com)
// - https://github.com/github/github/blob/master/app/api/codespaces.rb (for vscs_internal)
// TODO(adonovan): replace the last link with a public doc URL when available.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

const (
	VSCSTargetLocal       = "local"
	VSCSTargetDevelopment = "development"
	VSCSTargetPPE         = "ppe"
	VSCSTargetProduction  = "production"
)

// API is the interface to the codespace service.
type API struct {
	client         *http.Client
	externalClient *http.Client
	githubAPI      string
	retryBackoff   time.Duration
}

// New creates a new API client connecting to apiURL. client carries GitHub
// credentials and is used for requests to apiURL. externalClient is used for
// requests to non-GitHub hosts and must not carry GitHub credentials.
func New(client, externalClient *http.Client, apiURL string) *API {
	return &API{
		client:         client,
		externalClient: externalClient,
		githubAPI:      strings.TrimSuffix(apiURL, "/"),
		retryBackoff:   100 * time.Millisecond,
	}
}

// User represents a GitHub user.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// RepositoryOwner represents owner of a repository
type RepositoryOwner struct {
	Type  string `json:"type"`
	Login string `json:"login"`
}

// Repository represents a GitHub repository.
type Repository struct {
	ID            int64           `json:"id"`
	FullName      string          `json:"full_name"`
	DefaultBranch string          `json:"default_branch"`
	Owner         RepositoryOwner `json:"owner"`
}

// Codespace represents a codespace.
// You can see more about the fields in this type in the codespaces api docs:
// https://docs.github.com/en/rest/reference/codespaces
type Codespace struct {
	Name                           string              `json:"name"`
	CreatedAt                      string              `json:"created_at"`
	DisplayName                    string              `json:"display_name"`
	LastUsedAt                     string              `json:"last_used_at"`
	Owner                          User                `json:"owner"`
	Repository                     Repository          `json:"repository"`
	State                          string              `json:"state"`
	GitStatus                      CodespaceGitStatus  `json:"git_status"`
	Connection                     CodespaceConnection `json:"connection"`
	Machine                        CodespaceMachine    `json:"machine"`
	RuntimeConstraints             RuntimeConstraints  `json:"runtime_constraints"`
	VSCSTarget                     string              `json:"vscs_target"`
	PendingOperation               bool                `json:"pending_operation"`
	PendingOperationDisabledReason string              `json:"pending_operation_disabled_reason"`
	IdleTimeoutNotice              string              `json:"idle_timeout_notice"`
	WebURL                         string              `json:"web_url"`
	DevContainerPath               string              `json:"devcontainer_path"`
	Prebuild                       bool                `json:"prebuild"`
	Location                       string              `json:"location"`
	IdleTimeoutMinutes             int                 `json:"idle_timeout_minutes"`
	RetentionPeriodMinutes         int                 `json:"retention_period_minutes"`
	RetentionExpiresAt             string              `json:"retention_expires_at"`
	RecentFolders                  []string            `json:"recent_folders"`
	BillableOwner                  User                `json:"billable_owner"`
	EnvironmentId                  string              `json:"environment_id"`
}

type CodespaceGitStatus struct {
	Ahead                 int    `json:"ahead"`
	Behind                int    `json:"behind"`
	Ref                   string `json:"ref"`
	HasUnpushedChanges    bool   `json:"has_unpushed_changes"`
	HasUncommittedChanges bool   `json:"has_uncommitted_changes"`
}

type CodespaceMachine struct {
	Name            string `json:"name"`
	DisplayName     string `json:"display_name"`
	OperatingSystem string `json:"operating_system"`
	StorageInBytes  uint64 `json:"storage_in_bytes"`
	MemoryInBytes   uint64 `json:"memory_in_bytes"`
	CPUCount        int    `json:"cpus"`
}

const (
	// CodespaceStateAvailable is the state for a running codespace environment.
	CodespaceStateAvailable = "Available"
	// CodespaceStateShutdown is the state for a shutdown codespace environment.
	CodespaceStateShutdown = "Shutdown"
	// CodespaceStateShuttingDown is the state for a shutting down codespace environment.
	CodespaceStateShuttingDown = "ShuttingDown"
	// CodespaceStateStarting is the state for a starting codespace environment.
	CodespaceStateStarting = "Starting"
	// CodespaceStateRebuilding is the state for a rebuilding codespace environment.
	CodespaceStateRebuilding = "Rebuilding"
)

type CodespaceConnection struct {
	TunnelProperties TunnelProperties `json:"tunnelProperties"`
}

type TunnelProperties struct {
	ConnectAccessToken     string `json:"connectAccessToken"`
	ManagePortsAccessToken string `json:"managePortsAccessToken"`
	ServiceUri             string `json:"serviceUri"`
	TunnelId               string `json:"tunnelId"`
	ClusterId              string `json:"clusterId"`
	Domain                 string `json:"domain"`
}

type RuntimeConstraints struct {
	AllowedPortPrivacySettings []string `json:"allowed_port_privacy_settings"`
}

func (c *Codespace) ExportData(fields []string) map[string]any {
	v := reflect.ValueOf(c).Elem()
	data := map[string]any{}

	for _, f := range fields {
		switch f {
		case "owner":
			data[f] = c.Owner.Login
		case "repository":
			data[f] = c.Repository.FullName
		case "machineName":
			data[f] = c.Machine.Name
		case "machineDisplayName":
			data[f] = c.Machine.DisplayName
		case "retentionPeriodDays":
			data[f] = c.RetentionPeriodMinutes / 1440
		case "gitStatus":
			data[f] = map[string]any{
				"ref":                   c.GitStatus.Ref,
				"hasUnpushedChanges":    c.GitStatus.HasUnpushedChanges,
				"hasUncommittedChanges": c.GitStatus.HasUncommittedChanges,
				"ahead":                 c.GitStatus.Ahead,
				"behind":                c.GitStatus.Behind,
			}
		case "vscsTarget":
			if c.VSCSTarget != "" && c.VSCSTarget != VSCSTargetProduction {
				data[f] = c.VSCSTarget
			}
		default:
			sf := v.FieldByNameFunc(func(s string) bool {
				return strings.EqualFold(f, s)
			})
			data[f] = sf.Interface()
		}
	}

	return data
}

// GetCodespace returns the user codespace based on the provided name.
// If the codespace is not found, an error is returned.
// If includeConnection is true, it will return the connection information for the codespace.
func (a *API) GetCodespace(ctx context.Context, codespaceName string, includeConnection bool) (*Codespace, error) {
	u, err := a.codespaceURL(codespaceName)
	if err != nil {
		return nil, err
	}
	resp, err := a.withRetry(func() (*http.Response, error) {
		req, err := http.NewRequest(
			http.MethodGet,
			u,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating request: %w", err)
		}
		if includeConnection {
			q := req.URL.Query()
			q.Add("internal", "true")
			q.Add("refresh", "true")
			req.URL.RawQuery = q.Encode()
		}
		a.setHeaders(req)
		return a.do(ctx, req)
	})
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, handleHTTPError(resp)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	var response Codespace
	if err := json.Unmarshal(b, &response); err != nil {
		return nil, fmt.Errorf("error unmarshalling response: %w", err)
	}

	return &response, nil
}

// StartCodespace starts a codespace for the user.
// If the codespace is already running, the returned error from the API is ignored.
func (a *API) StartCodespace(ctx context.Context, codespaceName string) error {
	u, err := a.codespaceURL(codespaceName, "start")
	if err != nil {
		return err
	}
	resp, err := a.withRetry(func() (*http.Response, error) {
		req, err := http.NewRequest(
			http.MethodPost,
			u,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating request: %w", err)
		}
		a.setHeaders(req)
		return a.do(ctx, req)
	})
	if err != nil {
		return fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusConflict {
			// 409 means the codespace is already running which we can safely ignore
			return nil
		}
		return handleHTTPError(resp)
	}

	return nil
}

// do executes the given request and returns the response.
func (a *API) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	// TODO(adonovan): use NewRequestWithContext(ctx) and drop ctx parameter.
	req = req.WithContext(ctx)

	return a.client.Do(req)
}

// setHeaders sets the required headers for the API.
func (a *API) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github.v3+json")
}

// withRetry takes a generic function that sends an http request and retries
// only when the returned response has a >=500 status code.
func (a *API) withRetry(f func() (*http.Response, error)) (*http.Response, error) {
	bo := backoff.NewConstantBackOff(a.retryBackoff)
	return backoff.RetryWithData(func() (*http.Response, error) {
		resp, err := f()
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		if resp.StatusCode < 500 {
			return resp, nil
		}
		return nil, fmt.Errorf("received response with status code %d", resp.StatusCode)
	}, backoff.WithMaxRetries(bo, 3))
}

// ExternalHTTPClient returns an HTTP client for requests to non-GitHub hosts.
// It must not carry GitHub authentication credentials.
func (a *API) ExternalHTTPClient() (*http.Client, error) {
	return a.externalClient, nil
}

var codespaceNameRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// codespaceURL returns the API URL for the named codespace, followed by any
// further path elements. It rejects names that are not made of letters,
// digits and hyphens, because url.JoinPath cleans ".." and would otherwise let
// a name address another resource.
func (a *API) codespaceURL(codespaceName string, elem ...string) (string, error) {
	if !codespaceNameRE.MatchString(codespaceName) {
		return "", fmt.Errorf("invalid codespace name %q", codespaceName)
	}
	return url.JoinPath(a.githubAPI, append([]string{"user", "codespaces", codespaceName}, elem...)...)
}

// StatusError is returned when the API replies with an unexpected status code.
type StatusError struct {
	StatusCode int
	// Message is the "message" field of the JSON response body when present,
	// otherwise the status line (resp.Status), as go-gh does.
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}

// handleHTTPError returns a StatusError for the response. The caller is
// responsible for closing the response body.
func handleHTTPError(resp *http.Response) error {
	statusErr := &StatusError{StatusCode: resp.StatusCode, Message: resp.Status}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return statusErr
	}
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		statusErr.Message = parsed.Message
	}

	return statusErr
}
