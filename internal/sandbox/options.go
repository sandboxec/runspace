//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package sandbox

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.sandbox.ec/runspace/internal/github"
)

// Defaults shared by the CLI and MCP. The CLI registers them as flag
// defaults, and Validate applies them to empty MCP fields.
const (
	DefaultTimeout   = 2 * time.Hour
	DefaultKeep      = KeepNever
	DefaultIdle      = 10 * time.Minute
	DefaultRetention = 24 * time.Hour
	DefaultMachine   = "basicLinux32gb"
)

// Values of --keep.
const (
	KeepNever   = "never"
	KeepFailure = "failure"
	KeepAlways  = "always"
)

// DisplayName tags every codespace runspace creates. runspace acts only on
// codespaces with this display name.
const DisplayName = "runspace"

var (
	envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	shaRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// The explanations that follow "invalid FIELD VALUE:" in a validation error.
// Front ends show these messages, so their wording is part of the interface.
var (
	errWantRepo        = errors.New("want OWNER/REPO")
	errWantSHA         = errors.New("want a full lowercase commit hash")
	errWantKeep        = errors.New("want never, failure, or always")
	errWantEnvKey      = errors.New("want letters, digits, and underscores, not starting with a digit")
	errWantEnvValue    = errors.New("want KEY=VALUE; a bare KEY is not copied from runspace's environment")
	errWantName        = errors.New("want letters, digits, and hyphens, as ls prints them")
	errWantDuration    = errors.New("want a duration such as 30m or 2h")
	errNegative        = errors.New("must not be negative")
	errNotWholeMinutes = errors.New("must be a whole number of minutes")
	errEnvCommandForm  = errors.New("with env set, it must not contain = or start with -; run it through sh -c")
)

var (
	errNoCommand = errors.New("no command given")
	errNUL       = errors.New("command, env, and workdir must not contain NUL bytes")
	errNoTarget  = errors.New("give sandbox names or all (--all)")
	errBothKinds = errors.New("give sandbox names or all (--all), not both")
)

// CreateOptions are the options of create, and the create half of run.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema descriptions are long.
type CreateOptions struct {
	Repo         string `flag:"repo"         json:"repo"                   jsonschema:"repository to create the sandbox from, as OWNER/REPO"`
	Branch       string `flag:"branch"       json:"branch,omitempty"       jsonschema:"branch to check out; the repository default when empty"`
	SHA          string `flag:"sha"          json:"sha,omitempty"          jsonschema:"full commit hash that HEAD must equal, or creation fails"`
	Machine      string `flag:"machine"      json:"machine,omitempty"      jsonschema:"machine type (default basicLinux32gb, 2 cores)"`
	Devcontainer string `flag:"devcontainer" json:"devcontainer,omitempty" jsonschema:"path of the devcontainer.json to use; the repository default when empty"`
	Idle         string `flag:"idle"         json:"idle,omitempty"         jsonschema:"idle timeout in whole minutes, as a Go duration such as 30m (default 10m)"`
	Retention    string `flag:"retention"    json:"retention,omitempty"    jsonschema:"how long a stopped sandbox is kept before deletion, in whole minutes, as a Go duration such as 1h; 0 deletes at once (default 24h)"`

	idle, retention time.Duration
}

// CommandOptions describe one remote command. ExecOptions and RunOptions
// share them.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema descriptions are long.
type CommandOptions struct {
	TimeoutOption

	Argv    []string `arg:"CMD [ARG...]" json:"argv"              jsonschema:"command and arguments; each element is one word, never split or interpreted by a shell"`
	Env     []string `flag:"env"         json:"env,omitempty"     jsonschema:"environment for the command, as KEY=VALUE"`
	Workdir string   `flag:"workdir"     json:"workdir,omitempty" jsonschema:"working directory; relative paths are joined to /workspaces/REPO (the default)"`
}

// ExecOptions are the options of exec.
type ExecOptions struct {
	CommandOptions

	Name string `arg:"NAME" json:"name" jsonschema:"sandbox name"`
}

// RunOptions are the options of run: create, run one command, and delete or
// stop the sandbox.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema descriptions are long.
type RunOptions struct {
	CreateOptions
	CommandOptions

	Keep string `flag:"keep" json:"keep,omitempty" jsonschema:"when to keep the sandbox, stopped, instead of deleting it: never, failure (non-zero exit), or always (default never)"`
}

// DiffOptions are the options of diff.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema descriptions are long.
type DiffOptions struct {
	TimeoutOption

	Name   string `arg:"NAME"    json:"name"             jsonschema:"sandbox name"`
	Output string `flag:"output" json:"output,omitempty" jsonschema:"host file to write the patch to, atomically; relative to the working directory"`
}

// CopyOptions are the options of cp. Exactly one of Src and Dst is remote,
// written NAME:PATH.
//
//nolint:lll,tagliatelle // A struct tag cannot be wrapped; follow_link is the MCP name of --follow-link.
type CopyOptions struct {
	TimeoutOption

	Src        string `arg:"SRC"          json:"src"                   jsonschema:"source: a host path, or NAME:PATH in a sandbox"`
	Dst        string `arg:"DST"          json:"dst"                   jsonschema:"destination, which names the result: a host path, or NAME:PATH in a sandbox"`
	FollowLink bool   `flag:"follow-link" json:"follow_link,omitempty" jsonschema:"follow symbolic links in the source"`

	// srcLoc and dstLoc are Src and Dst as Validate parsed them.
	srcLoc, dstLoc location
}

// ListOptions are the options of ls. It has none: its flags only choose the
// output format.
type ListOptions struct{}

// DeleteOptions are the options of delete.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema descriptions are long.
type DeleteOptions struct {
	Names []string `arg:"NAME..." json:"names,omitempty" jsonschema:"sandboxes to delete"`
	All   bool     `flag:"all"    json:"all,omitempty"   jsonschema:"delete every sandbox runspace created for this identity"`
}

// ValidRepo reports whether repo is OWNER/REPO with names GitHub allows.
func ValidRepo(repo string) bool { return github.ValidRepo(repo) }

// Validate checks the options and applies defaults.
func (o *CreateOptions) Validate() error {
	if !github.ValidRepo(o.Repo) {
		return fmt.Errorf("invalid repo %q: %w", o.Repo, errWantRepo)
	}

	if o.SHA != "" && !shaRE.MatchString(o.SHA) {
		return fmt.Errorf("invalid sha %q: %w", o.SHA, errWantSHA)
	}

	if o.Machine == "" {
		o.Machine = DefaultMachine
	}

	var err error

	o.idle, err = minutes("idle", o.Idle, DefaultIdle)
	if err != nil {
		return err
	}

	o.retention, err = minutes("retention", o.Retention, DefaultRetention)

	return err
}

// Validate checks the options and applies defaults.
func (o *CommandOptions) Validate() error {
	if len(o.Argv) == 0 {
		return errNoCommand
	}
	// A NUL byte would silently truncate the remote command line.
	if slices.ContainsFunc(o.Argv, hasNUL) || slices.ContainsFunc(o.Env, hasNUL) || hasNUL(o.Workdir) {
		return errNUL
	}
	// With env set, the command runs as env's operand, and env would read a
	// name containing = as an assignment or one starting with - as an option.
	if len(o.Env) > 0 && (strings.Contains(o.Argv[0], "=") || strings.HasPrefix(o.Argv[0], "-")) {
		return fmt.Errorf("invalid command %q: %w", o.Argv[0], errEnvCommandForm)
	}

	err := validEnv(o.Env)
	if err != nil {
		return err
	}

	return o.parse()
}

// validEnv checks env entries. A bare KEY is refused rather than copied from
// runspace's environment, so a variable a front end inherits, such as a model
// API key, reaches a sandbox only when the caller already has its value.
func validEnv(entries []string) error {
	for _, entry := range entries {
		key, _, hasValue := strings.Cut(entry, "=")
		if !envRE.MatchString(key) {
			return fmt.Errorf("invalid env key %q: %w", key, errWantEnvKey)
		}

		if !hasValue {
			return fmt.Errorf("invalid env %q: %w", entry, errWantEnvValue)
		}
	}

	return nil
}

// Validate checks the options and applies defaults.
func (o *ExecOptions) Validate() error {
	err := validName(o.Name)
	if err != nil {
		return err
	}

	return o.CommandOptions.Validate()
}

// Validate checks the options and applies defaults.
func (o *RunOptions) Validate() error {
	err := o.CreateOptions.Validate()
	if err != nil {
		return err
	}

	err = o.CommandOptions.Validate()
	if err != nil {
		return err
	}

	switch o.Keep {
	case "":
		o.Keep = DefaultKeep
	case KeepNever, KeepFailure, KeepAlways:
	default:
		return fmt.Errorf("invalid keep %q: %w", o.Keep, errWantKeep)
	}

	return nil
}

// Validate checks the options and applies defaults.
func (o *DiffOptions) Validate() error {
	err := validName(o.Name)
	if err != nil {
		return err
	}

	return o.parse()
}

// Validate checks that exactly one side is remote and applies defaults.
func (o *CopyOptions) Validate() error {
	var err error

	o.srcLoc, o.dstLoc, err = o.locations()
	if err != nil {
		return err
	}

	return o.parse()
}

// Validate checks that either names or all is given, never both.
func (o *DeleteOptions) Validate() error {
	switch {
	case o.All && len(o.Names) > 0:
		return errBothKinds
	case !o.All && len(o.Names) == 0:
		return errNoTarget
	}

	for _, name := range o.Names {
		err := validName(name)
		if err != nil {
			return err
		}
	}

	return nil
}

func hasNUL(text string) bool { return strings.IndexByte(text, 0) >= 0 }

func validName(name string) error {
	if !github.ValidCodespaceName(name) {
		return fmt.Errorf("invalid sandbox name %q: %w", name, errWantName)
	}

	return nil
}

// duration parses a Go duration, or returns fallback for "". Negative values
// are rejected.
func duration(field, text string, fallback time.Duration) (time.Duration, error) {
	if text == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, text, errWantDuration)
	}

	if parsed < 0 {
		return 0, fmt.Errorf("invalid %s %q: %w", field, text, errNegative)
	}

	return parsed, nil
}

// minutes is duration restricted to whole minutes, so 30s is rejected rather
// than truncated to 0.
func minutes(field, text string, fallback time.Duration) (time.Duration, error) {
	parsed, err := duration(field, text, fallback)
	if err != nil {
		return 0, err
	}

	if parsed%time.Minute != 0 {
		return 0, fmt.Errorf("invalid %s %q: %w", field, text, errNotWholeMinutes)
	}

	return parsed, nil
}

// FormatDuration writes d without zero units: 2h, 10m, 1h30m.
func FormatDuration(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}

	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}

	return text
}
