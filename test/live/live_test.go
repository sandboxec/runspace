//go:build live

// Package live holds checks against real codespaces. They need
// RUNSPACE_TOKEN and a throwaway repository (RUNSPACE_LIVE_REPO), and they
// create billable codespaces. Run them with -tags live -count=1; -short
// skips the waits for idle timeouts.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sandbox.ec/runspace/internal/github"
)

// defaultRepo is the throwaway sandbox repository for live checks.
const defaultRepo = "dwisiswant0/runspace-sandbox"

// liveRepo returns RUNSPACE_LIVE_REPO, or defaultRepo.
func liveRepo() string {
	if r := os.Getenv("RUNSPACE_LIVE_REPO"); r != "" {
		return r
	}
	return defaultRepo
}

// liveRepoName returns the repository name without its owner.
func liveRepoName() string {
	_, name, _ := strings.Cut(liveRepo(), "/")
	return name
}

// bin is the runspace binary under test.
var bin string

func TestMain(m *testing.M) {
	flag.Parse()
	// -list only names the checks, so it must not build or delete anything.
	if flag.Lookup("test.list").Value.String() != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "runspace-live-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "runspace")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "go.sandbox.ec/runspace")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	code := m.Run()
	// A run that shares the repository with another live run sets
	// RUNSPACE_LIVE_NO_CLEANUP and deletes its own sandboxes.
	if os.Getenv("RUNSPACE_LIVE_NO_CLEANUP") == "" {
		cleanup()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// cleanup deletes every codespace left in the live repository, tagged or
// not, with gh, so it also removes the untagged one that TestRefusesUntagged
// makes.
func cleanup() {
	out, err := exec.Command("gh", "codespace", "list", "-R", liveRepo(), "--json", "name", "--jq", ".[].name").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cleanup: list:", err)
		return
	}
	for name := range strings.FieldsSeq(string(out)) {
		fmt.Fprintln(os.Stderr, "cleanup: deleting", name)
		exec.Command("gh", "codespace", "delete", "-c", name, "--force").Run()
	}
}

// result is one runspace invocation.
type result struct {
	stdout, stderr string
	code           int
	took           time.Duration
}

// runspace runs the binary with runspace's environment plus extra.
func runspace(t *testing.T, stdin string, extra []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"RUNSPACE_TOKEN=" + os.Getenv("RUNSPACE_TOKEN"),
	}, extra...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String(), took: time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("runspace %q: %v", args, err)
	}
	t.Logf("runspace %s → %d in %v\n%s", strings.Join(args, " "), r.code, r.took.Round(time.Millisecond), indent(r.stderr))
	return r
}

// logValue returns the value of key in the first log line that contains
// msg, or "".
func logValue(log, msg, key string) string {
	for line := range strings.Lines(log) {
		if !strings.Contains(line, msg) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(field, key+"="); ok {
				return value
			}
		}
	}
	return ""
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

var (
	sharedOnce sync.Once
	sharedName string
	sharedErr  error
)

// shared returns one default sandbox that several checks reuse, so a run
// pays for one provisioning. cleanup deletes it.
func shared(t *testing.T) string {
	t.Helper()
	sharedOnce.Do(func() {
		r := runspace(t, "", nil, "create", "-R", liveRepo(), "--json")
		if r.code != 0 {
			sharedErr = fmt.Errorf("create: exit %d: %s", r.code, r.stderr)
			return
		}
		var res struct{ Name, Base string }
		sharedErr = json.Unmarshal([]byte(r.stdout), &res)
		sharedName = res.Name
		t.Logf("shared sandbox %s at %s (create took %v)", res.Name, res.Base, r.took)
	})
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	return sharedName
}

// sandboxSlots bounds how many checks with their own sandbox run at once.
// GitHub refuses a create while an account runs too many codespaces: one
// create was refused with four or five running. With two slots and the
// shared sandbox, a create starts with at most three, counting one that is
// still being deleted, since delete returns before the codespace is gone.
var sandboxSlots = make(chan struct{}, 2)

// ownSandbox runs t in parallel with the other checks that create their own
// sandbox, once one of sandboxSlots is free.
func ownSandbox(t *testing.T) {
	t.Helper()
	t.Parallel()
	sandboxSlots <- struct{}{}
	t.Cleanup(func() { <-sandboxSlots })
}

// linuxBinary builds a static runspace for linux/goarch, to run in a
// container or a codespace rather than on this host.
func linuxBinary(t *testing.T, goarch string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "runspace")
	//nolint:gosec // G204: the check builds runspace into its own temporary directory.
	build := exec.CommandContext(t.Context(), "go", "build", "-o", path, "go.sandbox.ec/runspace")

	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)

	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build for linux/%s: %v: %s", goarch, err, out)
	}

	return path
}

// api returns a documented-API client for assertions.
func api(t *testing.T) *github.Client {
	t.Helper()
	token, err := github.Token("", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := github.NewClients(token, github.DefaultAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	return github.NewClient(c, github.DefaultAPIURL)
}
