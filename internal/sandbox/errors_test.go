//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"go.sandbox.ec/runspace/internal/sandbox"
)

type apiErrorCase struct {
	name string
	fail string                                         // the "METHOD /path" the fake GitHub fails
	call func(ctx context.Context, rig *fixture) string // returns the error text
	want string                                         // the start of the error text
}

// message returns the text of err, or "" for nil.
func message(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func apiErrorCases() []apiErrorCase {
	return []apiErrorCase{
		{"ls", "GET /user/codespaces", func(ctx context.Context, rig *fixture) string {
			_, err := rig.List(ctx, sandbox.ListOptions{})

			return message(err)
		}, "list codespaces: "},
		// Delete looks the codespace up first; exec leaves that to the dial.
		{"lookup", "GET /user/codespaces/cs-1", func(ctx context.Context, rig *fixture) string {
			_, err := rig.Delete(ctx, newDelete(testName))

			return message(err)
		}, "get codespace cs-1: "},
		{"create", "POST /repos/o/r/codespaces", func(ctx context.Context, rig *fixture) string {
			_, err := rig.Create(ctx, newCreateOptions(), nil)

			return message(err)
		}, "create codespace in o/r: "},
		{"delete", deleteCS1, func(ctx context.Context, rig *fixture) string {
			_, err := rig.Delete(ctx, newDelete(testName))

			return message(err)
		}, "delete codespace cs-1: "},
	}
}

// A GitHub API failure reaches the caller with the API client's message
// first: the CLI and MCP print it as is, so no layer adds its own prefix.
func TestAPIErrorsKeepTheirText(t *testing.T) {
	t.Parallel()

	for _, test := range apiErrorCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			rig := newFixture(t, false)

			if test.name != "create" {
				rig.mustCreate(ctx, t)
			}

			rig.gh.Fail = map[string]int{test.fail: 500}

			if got := test.call(ctx, rig); !strings.HasPrefix(got, test.want) {
				t.Errorf("error = %q, want it to start with %q", got, test.want)
			}
		})
	}
}
