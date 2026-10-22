//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.sandbox.ec/runspace/internal/sandbox"
)

const (
	errNoName      = "invalid sandbox name"
	remoteCS1PathA = "cs-1:a"
)

// validator is what every options struct implements.
type validator interface{ Validate() error }

type rejectCase struct {
	name string
	opts validator
	want string
}

// rejectCases lists what neither front end may pass.
func rejectCases() []rejectCase {
	cmd := newCommand(cmdTrue)
	nameless := newExec("", cmd)
	badTimeout := newTimeout("soon")
	copyTimeout := newCopy(remoteCS1PathA, "b")
	copyTimeout.TimeoutOption = badTimeout
	diffTimeout := newDiff(testName, "")
	diffTimeout.TimeoutOption = newTimeout("-1m")
	keepRun := newRun(cmd, "sometimes")
	emptyRun := newRun(newCommand(), "")

	return []rejectCase{
		{"env key", new(newExec(testName, withEnv(cmd, "1A=x"))), `invalid env key "1A"`},
		{"env key with dash", new(newExec(testName, withEnv(cmd, "A-B"))), `invalid env key "A-B"`},
		{"env without a value", new(newExec(testName, withEnv(cmd, "HOME"))), `invalid env "HOME": want KEY=VALUE`},
		{"keep", &keepRun, `invalid keep "sometimes"`},
		{"idle seconds", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.Idle = "90s" })), "whole number of minutes"},
		{"retention seconds", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.Retention = "30s" })),
			"whole number of minutes"},
		{"retention negative", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.Retention = "-1m" })),
			"must not be negative"},
		{"timeout syntax", new(newExec(testName, withTimeout(cmd, "soon"))), `invalid timeout "soon"`},
		{"cp two remote", new(newCopy(remoteCS1PathA, "cs-2:b")), "must be a sandbox path"},
		{"cp no remote", new(newCopy("a", "./b:c")), "must be a sandbox path"},
		{"cp empty host", new(newCopy(remoteCS1PathA, "")), "must not be empty"},
		{"empty argv", new(newExec(testName, newCommand())), "no command given"},
		{"cp timeout", &copyTimeout, `invalid timeout "soon"`},
		{"diff timeout", &diffTimeout, "must not be negative"},
		{"NUL in argv", new(newExec(testName, newCommand("a\x00b"))), "NUL"},
		{"env with an = command", new(newExec(testName, withEnv(newCommand("A=1"), "B=2"))), "must not contain ="},
		{"NUL in env", new(newExec(testName, withEnv(newCommand("x"), "A=\x00"))), "NUL"},
		{"run empty argv", &emptyRun, "no command given"},
		{"empty name", &nameless, errNoName},
		{"bad name", new(newDiff("../x", "")), errNoName},
		{"repo", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.Repo = "o" })), "invalid repo"},
		{"repo dots", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.Repo = "o/.." })), "invalid repo"},
		{"sha", new(newCreateOptions(func(o *sandbox.CreateOptions) { o.SHA = "abc123" })), "invalid sha"},
		{"delete neither", new(newDelete()), "give sandbox names or all"},
		{"delete both", &sandbox.DeleteOptions{Names: []string{testName}, All: true}, "not both"},
		{"delete bad name", new(newDelete("cs 1")), errNoName},
	}
}

// Validate rejects what neither front end may pass.
func TestValidateRejects(t *testing.T) {
	t.Parallel()

	for _, test := range rejectCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.opts.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Validate() = %v, want an error containing %q", err, test.want)
			}
		})
	}
}

func TestValidateDefaultsRun(t *testing.T) {
	t.Parallel()

	opts := newRun(newCommand(cmdTrue), "")
	opts.Repo = "o/.github"
	opts.Retention = "0"

	err := opts.Validate()
	if err != nil {
		t.Fatal(err)
	}

	if opts.Machine != sandbox.DefaultMachine || opts.ParsedIdle() != sandbox.DefaultIdle ||
		opts.ParsedRetention() != 0 || opts.ParsedTimeout() != sandbox.DefaultTimeout ||
		opts.Keep != sandbox.DefaultKeep {
		t.Errorf("defaults = %q %v %v %v %q",
			opts.Machine, opts.ParsedIdle(), opts.ParsedRetention(), opts.ParsedTimeout(), opts.Keep)
	}
}

func TestValidateTimeoutZero(t *testing.T) {
	t.Parallel()

	opts := newExec(testName, withTimeout(newCommand("x"), "0"))

	err := opts.Validate()
	if err != nil || opts.ParsedTimeout() != 0 {
		t.Errorf("timeout 0: %v, %v", opts.ParsedTimeout(), err)
	}
}

func TestValidateDiffDefaultTimeout(t *testing.T) {
	t.Parallel()

	opts := newDiff(testName, "")

	err := opts.Validate()
	if err != nil || opts.ParsedTimeout() != sandbox.DefaultTimeout {
		t.Errorf("diff default timeout: %v, %v", opts.ParsedTimeout(), err)
	}
}

func TestValidateCopyAcceptsOneRemoteSide(t *testing.T) {
	t.Parallel()

	for _, opts := range []sandbox.CopyOptions{
		newCopy("a", "cs-1:b"), newCopy("cs-1:", "-"), newCopy("-", "cs-1:dir"), newCopy("./x:y", "cs-1:b"),
	} {
		err := opts.Validate()
		if err != nil {
			t.Errorf("%+v: %v", opts, err)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	for dur, want := range map[time.Duration]string{
		2 * time.Hour:           "2h",
		10 * time.Minute:        "10m",
		90 * time.Minute:        "1h30m",
		0:                       "0s",
		1500 * time.Millisecond: "1.5s",
	} {
		got := sandbox.FormatDuration(dur)
		if got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", dur, got, want)
		}
	}
}

// The defaults the MCP schema describes are the named constants.
func TestSchemaDescriptionsMatchDefaults(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"CreateOptions.Machine":   sandbox.DefaultMachine,
		"CreateOptions.Idle":      sandbox.FormatDuration(sandbox.DefaultIdle),
		"CreateOptions.Retention": sandbox.FormatDuration(sandbox.DefaultRetention),
		"TimeoutOption.Timeout":   sandbox.FormatDuration(sandbox.DefaultTimeout),
		"RunOptions.Keep":         sandbox.DefaultKeep,
	}

	for _, typ := range []reflect.Type{
		reflect.TypeFor[sandbox.CreateOptions](),
		reflect.TypeFor[sandbox.TimeoutOption](),
		reflect.TypeFor[sandbox.RunOptions](),
	} {
		for field := range typ.Fields() {
			def, ok := want[typ.Name()+"."+field.Name]
			if !ok {
				continue
			}

			desc := field.Tag.Get("jsonschema")
			if !strings.Contains(desc, "(default "+def) {
				t.Errorf("%s.%s describes %q, want default %s", typ.Name(), field.Name, desc, def)
			}
		}
	}
}
