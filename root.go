//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"errors"
	"runtime/debug"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// tokenFlag is the name of the -T flag, which every command takes.
const tokenFlag = "token"

var errEmptyToken = errors.New("--token is empty; pass a token, or leave it out to use RUNSPACE_TOKEN")

func newRoot(cli *app) *cobra.Command {
	root := new(cobra.Command)
	root.Use = "runspace"
	root.Short = "runspace runs commands in disposable GitHub codespaces"
	root.Long = `runspace

Disposable cloud sandboxes for humans and AI agents
powered by GitHub Codespaces.`
	root.Version = version()
	// main logs each error once.
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetIn(cli.stdin)
	root.SetOut(cli.stdout)
	root.SetErr(cli.stderr)
	// Not -t, which exec takes for a terminal, as in exec -it.
	root.PersistentFlags().StringVarP(&cli.token, tokenFlag, "T", "",
		"GitHub token with the codespace scope, used instead of RUNSPACE_TOKEN")
	root.PersistentFlags().BoolVarP(&cli.verbose, verboseFlag, "v", false,
		"log debug messages; wins over "+envLogLevel)
	// -V, as -v is --verbose. Cobra adds no version flag of its own when
	// one is defined.
	root.Flags().BoolP("version", "V", false, "version for runspace")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		err := cli.configureLog(cmd)
		if err != nil {
			return err
		}

		// An empty --token, as from an unset variable, fails rather than
		// fall back to RUNSPACE_TOKEN, which may be another identity.
		if cli.token == "" && cmd.Flags().Changed(tokenFlag) {
			return errEmptyToken
		}

		return nil
	}
	root.AddCommand(
		newCreateCmd(cli),
		newExecCmd(cli),
		newRunCmd(cli),
		newDiffCmd(cli),
		newCopyCmd(cli),
		newListCmd(cli),
		newDeleteCmd(cli),
		newMCPCmd(cli),
	)

	return root
}

// version returns the module version from the build info: the tag for a
// release or go install build, a pseudo-version for a local build.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" {
		return info.Main.Version
	}

	return "(devel)"
}

// addCreateFlags registers create's options, shared with run.
func addCreateFlags(flags *pflag.FlagSet, opts *sandbox.CreateOptions) {
	flags.StringVarP(&opts.Repo, "repo", "R", "", "repository to create the sandbox from, as OWNER/REPO "+
		"(default: the git remote origin of the current directory)")
	flags.StringVarP(&opts.Branch, "branch", "b", "", "branch to check out (default: the repository's default branch)")
	flags.StringVar(&opts.SHA, "sha", "", "fail unless HEAD is this full commit hash")
	flags.StringVarP(&opts.Machine, "machine", "m", sandbox.DefaultMachine, "machine type")
	flags.StringVar(&opts.Devcontainer, "devcontainer", "",
		"path of the devcontainer.json to use (default: the repository's)")
	flags.StringVar(&opts.Idle, "idle", sandbox.FormatDuration(sandbox.DefaultIdle),
		"idle timeout, a whole number of minutes such as 30m")
	flags.StringVar(&opts.Retention, "retention", sandbox.FormatDuration(sandbox.DefaultRetention),
		"how long a stopped sandbox is kept before deletion, a whole number of minutes such as 1h; 0 deletes it at once")
}

// addCommandFlags registers the options of one remote command, shared by
// exec and run.
func addCommandFlags(flags *pflag.FlagSet, opts *sandbox.CommandOptions) {
	// A string array, so a comma inside a value never splits it.
	flags.StringArrayVarP(&opts.Env, "env", "e", nil,
		"set KEY=VALUE for the command (repeatable)")
	flags.StringVarP(&opts.Workdir, "workdir", "w", "",
		"working directory; relative paths are joined to /workspaces/REPO")
	addTimeoutFlag(flags, &opts.TimeoutOption, "the command")
}

// addTimeoutFlag registers --timeout, shared by exec, run, cp, and diff.
func addTimeoutFlag(flags *pflag.FlagSet, opts *sandbox.TimeoutOption, what string) {
	flags.StringVar(&opts.Timeout, "timeout", sandbox.FormatDuration(sandbox.DefaultTimeout),
		"deadline for "+what+"; 0 disables it")
}
