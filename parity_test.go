//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// field is one option of a command, from its struct tags.
type field struct {
	json     string // MCP field name
	flag     string // long flag name, or ""
	arg      string // positional in the Use line, or ""
	required bool   // no omitempty
}

// optionFields flattens the fields of an options struct, including
// embedded structs, as encoding/json and the MCP schema do.
func optionFields(typ reflect.Type) []field {
	var out []field

	for _, structField := range reflect.VisibleFields(typ) {
		if structField.Anonymous || !structField.IsExported() {
			continue
		}

		name, opts, _ := strings.Cut(structField.Tag.Get("json"), ",")
		out = append(out, field{
			json:     name,
			flag:     structField.Tag.Get("flag"),
			arg:      structField.Tag.Get("arg"),
			required: !strings.Contains(opts, "omitempty"),
		})
	}

	return out
}

// commandOptions maps each command to its options struct.
func commandOptions() map[string]reflect.Type {
	return map[string]reflect.Type{
		cmdCreate: reflect.TypeFor[sandbox.CreateOptions](),
		cmdExec:   reflect.TypeFor[sandbox.ExecOptions](),
		cmdRun:    reflect.TypeFor[sandbox.RunOptions](),
		cmdDiff:   reflect.TypeFor[sandbox.DiffOptions](),
		cmdCopy:   reflect.TypeFor[sandbox.CopyOptions](),
		cmdList:   reflect.TypeFor[sandbox.ListOptions](),
		cmdDelete: reflect.TypeFor[sandbox.DeleteOptions](),
	}
}

// commandNames lists the seven commands, sorted.
func commandNames() []string {
	return []string{cmdCopy, cmdCreate, cmdDelete, cmdDiff, cmdExec, cmdList, cmdRun}
}

// cliOnlyFlags is the CLI half of the transport list: stream and output
// format flags with no MCP field.
func cliOnlyFlags() map[string][]string {
	return map[string][]string{
		cmdCreate: {"json"},
		cmdExec:   {"interactive", "tty"},
		cmdRun:    {"interactive"},
		cmdList:   {"quiet", "json"},
	}
}

// positionalJSON maps Use-line positionals to MCP field names.
func positionalJSON() map[string]string {
	return map[string]string{
		"NAME":         "name",
		"CMD [ARG...]": "argv",
		"SRC":          "src",
		"DST":          "dst",
		"NAME...":      "names",
	}
}

// repoFlagName is the long name of the flag whose requiredness differs
// between the CLI and MCP.
const repoFlagName = "repo"

// emptyApp is an app with nothing attached, enough to build the command
// tree.
func emptyApp() *app {
	return &app{
		stdin: nil, stdout: nil, stderr: nil, tty: nil, log: nil, getenv: nil, verbose: false,
		dir: "", token: "", newSandboxes: nil,
	}
}

// Every flag and positional maps to an options field, and every options
// field to a flag or positional, except the transport list. Field names
// follow the long flag names.
func TestParity(t *testing.T) {
	t.Parallel()

	var names []string

	for _, cmd := range newRoot(emptyApp()).Commands() {
		if slices.Contains([]string{"help", "completion", "mcp"}, cmd.Name()) {
			continue
		}

		names = append(names, cmd.Name())

		typ, ok := commandOptions()[cmd.Name()]
		if !ok {
			t.Errorf("command %s has no options struct", cmd.Name())

			continue
		}

		checkCommand(t, cmd, optionFields(typ))
	}

	slices.Sort(names)

	if want := commandNames(); !slices.Equal(names, want) {
		t.Errorf("commands = %v, want %v", names, want)
	}
}

// checkCommand checks one command's flags and positionals against the
// fields of its options struct.
func checkCommand(t *testing.T, cmd *cobra.Command, fields []field) {
	t.Helper()

	flagFields := map[string]field{}

	var args []string

	for _, opt := range fields {
		switch {
		case opt.flag != "" && opt.arg != "", opt.flag == "" && opt.arg == "":
			t.Errorf("%s: field %s needs exactly one of flag and arg tags", cmd.Name(), opt.json)
		case opt.flag != "":
			flagFields[opt.flag] = opt

			checkFlagField(t, cmd, opt)
		default:
			args = append(args, opt.arg)

			checkArgField(t, cmd, opt)
		}
	}

	checkFlagsHaveFields(t, cmd, flagFields)
	checkTransportFlags(t, cmd)
	checkPositionals(t, cmd, args)
}

// checkFlagField checks that opt's flag exists, is named after its MCP
// field, and is required exactly when the field is.
func checkFlagField(t *testing.T, cmd *cobra.Command, opt field) {
	t.Helper()

	name := cmd.Name()

	if want := strings.ReplaceAll(opt.flag, "-", "_"); opt.json != want {
		t.Errorf("%s: --%s has MCP field %q, want %q", name, opt.flag, opt.json, want)
	}

	flagDef := cmd.Flags().Lookup(opt.flag)
	if flagDef == nil {
		t.Errorf("%s: no flag --%s for field %s", name, opt.flag, opt.json)

		return
	}

	got := isRequired(flagDef)

	// The one exception: the repo flag is optional on the CLI, which defaults
	// it to the git remote origin of the current directory, while the MCP
	// field stays required: an MCP server's working directory need not be
	// the caller's. The exception holds exactly: no other combination passes.
	if opt.flag == repoFlagName {
		if got || !opt.required {
			t.Errorf("%s: --%s required = %v, field required = %v; want an optional flag for a required field",
				name, opt.flag, got, opt.required)
		}

		return
	}

	if got != opt.required {
		t.Errorf("%s: --%s required = %v, field required = %v", name, opt.flag, got, opt.required)
	}
}

// checkArgField checks that opt's positional maps to its MCP field.
func checkArgField(t *testing.T, cmd *cobra.Command, opt field) {
	t.Helper()

	if want := positionalJSON()[opt.arg]; opt.json != want {
		t.Errorf("%s: positional %s has MCP field %q, want %q", cmd.Name(), opt.arg, opt.json, want)
	}
}

// checkFlagsHaveFields checks that every flag of cmd has an options field,
// except help and the transport list.
func checkFlagsHaveFields(t *testing.T, cmd *cobra.Command, flagFields map[string]field) {
	t.Helper()

	cmd.LocalFlags().VisitAll(func(flagDef *pflag.Flag) {
		if flagDef.Name == "help" || slices.Contains(cliOnlyFlags()[cmd.Name()], flagDef.Name) {
			return
		}

		if _, ok := flagFields[flagDef.Name]; !ok {
			t.Errorf("%s: flag --%s has no options field", cmd.Name(), flagDef.Name)
		}
	})
}

// checkTransportFlags checks that every CLI-only flag exists.
func checkTransportFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()

	for _, name := range cliOnlyFlags()[cmd.Name()] {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("%s: transport-list flag --%s is missing", cmd.Name(), name)
		}
	}
}

// checkPositionals checks that the Use line and the options name the same
// positionals. Their order is the CLI's to choose: each command assigns its
// arguments explicitly.
func checkPositionals(t *testing.T, cmd *cobra.Command, args []string) {
	t.Helper()

	rest := usePositionals(cmd)
	for _, arg := range args {
		if !strings.Contains(rest, arg) {
			t.Errorf("%s: options positional %q is not in the Use line %q", cmd.Name(), arg, cmd.Use)
		}

		rest = strings.Replace(rest, arg, "", 1)
	}

	if extra := strings.TrimSpace(rest); extra != "" {
		t.Errorf("%s: Use line positionals %q have no options field", cmd.Name(), extra)
	}
}

// usePositionals strips the command name, [flags], and flag mentions from
// the Use line.
func usePositionals(cmd *cobra.Command) string {
	use := strings.TrimPrefix(cmd.Use, cmd.Name())
	for _, mention := range []string{"[flags]", "[-R OWNER/REPO]", "| --all"} {
		use = strings.ReplaceAll(use, mention, "")
	}

	return strings.Join(strings.Fields(use), " ")
}

func isRequired(flagDef *pflag.Flag) bool {
	values := flagDef.Annotations[cobra.BashCompOneRequiredFlag]

	return len(values) > 0 && values[0] == "true"
}
