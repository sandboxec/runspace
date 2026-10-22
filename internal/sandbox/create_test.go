//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"go.sandbox.ec/runspace/internal/remote"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// keyRetention is the field of a create body that must always be sent.
const keyRetention = "retention_period_minutes"

type createBodyCase struct {
	name string
	opts sandbox.CreateOptions
	want map[string]any
}

func createBodyCases() []createBodyCase {
	return []createBodyCase{
		{
			"defaults", newCreateOptions(),
			map[string]any{"idle_timeout_minutes": 10.0, keyRetention: 1440.0, "machine": sandbox.DefaultMachine},
		},
		{
			"idle and retention",
			newCreateOptions(func(o *sandbox.CreateOptions) { o.Idle, o.Retention = "5m", "1h" }),
			map[string]any{"idle_timeout_minutes": 5.0, keyRetention: 60.0},
		},
		{
			"retention zero", newCreateOptions(func(o *sandbox.CreateOptions) { o.Retention = "0" }),
			map[string]any{keyRetention: 0.0},
		},
		{
			"branch, devcontainer, machine",
			newCreateOptions(func(o *sandbox.CreateOptions) {
				o.Branch, o.Devcontainer, o.Machine = "task-42", ".devcontainer/x.json", "standardLinux32gb"
			}),
			map[string]any{"ref": "task-42", "devcontainer_path": ".devcontainer/x.json", "machine": "standardLinux32gb"},
		},
	}
}

// checkCreateBody checks the body of a create request.
func checkCreateBody(t *testing.T, opts sandbox.CreateOptions, want, body map[string]any) {
	t.Helper()

	if body["display_name"] != sandbox.DisplayName || body["multi_repo_permissions_opt_out"] != true {
		t.Errorf("body = %v, want the runspace tag and the multi-repo opt-out", body)
	}

	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %v, want %v", key, body[key], value)
		}
	}

	if _, ok := body[keyRetention]; !ok {
		t.Error("retention_period_minutes not sent")
	}

	if _, ok := body["devcontainer_path"]; ok && opts.Devcontainer == "" {
		t.Error("devcontainer_path sent while unset")
	}

	if _, ok := body["ref"]; ok && opts.Branch == "" {
		t.Error("ref sent while unset")
	}
}

// Every create body carries the runspace tag, the multi-repo
// opt-out, and explicit idle and retention, with retention 0 kept.
func TestCreateBody(t *testing.T) {
	t.Parallel()

	for _, test := range createBodyCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, false)

			res, err := rig.Create(context.Background(), test.opts, nil)
			if err != nil {
				t.Fatal(err)
			}

			if res.Base != sandboxtest.HeadSHA || res.Repository != testRepo || res.Workspace != testWorkspace {
				t.Errorf("result = %+v", res)
			}

			checkCreateBody(t, test.opts, test.want, rig.gh.Creates()[0])
		})
	}
}

// Create records the base with --sha checked.
func TestCreateSHA(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newFixture(t, false)

	_, err := rig.Create(ctx, newCreateOptions(func(o *sandbox.CreateOptions) { o.SHA = sandboxtest.HeadSHA }), nil)
	if err != nil {
		t.Fatal(err)
	}

	other := strings.Repeat("f", 40)

	_, err = rig.Create(ctx, newCreateOptions(func(o *sandbox.CreateOptions) { o.SHA = other }), nil)
	if err == nil || !strings.Contains(err.Error(), "not "+other) {
		t.Fatalf("mismatched --sha: %v", err)
	}

	if !rig.gh.Called("DELETE /user/codespaces/cs-2") {
		t.Error("mismatched sandbox not deleted")
	}

	prep := rig.dial.Conns()[0].Cmds()[0]
	script := prep.Argv[2]

	if sandboxtest.Step(prep) != stepPrepare || prep.Dir != testWorkspace || !strings.Contains(script, sandbox.BaseRef) {
		t.Errorf("prepare command = %+v", prep)
	}
}

// A failed or cancelled create deletes the codespace.
func TestCreateDialFailureDeletes(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	rig.dial.Err = errTunnelRefused

	_, err := rig.Create(context.Background(), newCreateOptions(), nil)
	if err == nil {
		t.Fatal("create succeeded")
	}

	if !rig.gh.Called(deleteCS1) {
		t.Error("not deleted")
	}
}

func TestCreatePrepareFailureDeletes(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	rig.dial.Handler = func(context.Context, string, remote.Cmd) (int, error) { return 128, nil }

	_, err := rig.Create(context.Background(), newCreateOptions(), nil)
	if err == nil {
		t.Fatal("create succeeded")
	}

	if !rig.gh.Called(deleteCS1) {
		t.Error("not deleted")
	}
}

func TestCreateCancelledWhileWaitingDeletes(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rig := newFixture(t, false)
		rig.gh.States = []string{stateProvisioning}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error)

		go func() {
			_, err := rig.Create(ctx, newCreateOptions(), nil)
			done <- err
		}()

		synctest.Wait()
		cancel()

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}

		if !rig.gh.Called(deleteCS1) {
			t.Error("cancelled create left its codespace")
		}
	})
}

func TestCreatePostFailureLeavesNothingToDelete(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, false)
	rig.gh.Fail = map[string]int{"POST /repos/o/r/codespaces": 400}

	_, err := rig.Create(context.Background(), newCreateOptions(), nil)
	if err == nil {
		t.Fatal("create succeeded")
	}

	for _, call := range rig.gh.Calls() {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("deleted although nothing was created: %s", call)
		}
	}
}

type availabilityCase struct {
	name   string
	states []string
	want   string // "" for success
}

func availabilityCases() []availabilityCase {
	return []availabilityCase{
		{"becomes available", []string{"Queued", stateProvisioning, "Available"}, ""},
		{"fails", []string{"Queued", "Failed"}, "is Failed"},
		{
			"times out", []string{stateProvisioning},
			"did not become available within 15m; its last state was Provisioning",
		},
	}
}

// The availability wait returns on Available, fails on Failed, and
// times out with the last state.
func TestAvailabilityWait(t *testing.T) {
	t.Parallel()

	for _, test := range availabilityCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rig := newFixture(t, false)
				rig.gh.States = test.states

				var lines []string

				_, err := rig.Create(context.Background(), newCreateOptions(), func(line string) { lines = append(lines, line) })
				if test.want == "" && err != nil {
					t.Errorf("%v: %v", test.states, err)
				}

				if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
					t.Errorf("%v: err = %v, want %q", test.states, err, test.want)
				}

				if test.want != "" && !rig.gh.Called(deleteCS1) {
					t.Errorf("%v: not deleted", test.states)
				}

				if len(lines) == 0 {
					t.Errorf("%v: no progress", test.states)
				}
			})
		})
	}
}
