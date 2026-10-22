//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"

	"charm.land/log/v2"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

func newRunCmd(cli *app) *cobra.Command {
	var (
		opts        sandbox.RunOptions
		interactive bool
	)

	cmd := new(cobra.Command)
	cmd.Use = "run [-R OWNER/REPO] [flags] CMD [ARG...]"
	cmd.Short = "Create a sandbox, run one command, and delete it"
	cmd.Long = `Create a sandbox, run one command in it, and delete it. With --keep, a sandbox
is stopped instead: always, or on failure (a non-zero exit). Flags come before
CMD. The exit code follows exec.

Without -R, OWNER/REPO comes from the git remote origin of the current
directory.`
	cmd.Args = cobra.MinimumNArgs(1)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts.Argv = args

		var err error

		opts.Repo, err = cli.repo(opts.Repo)
		if err != nil {
			return err
		}

		err = opts.Validate()
		if err != nil {
			return relay(err)
		}

		streams := cli.streams(cli.stdinIf(interactive))

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			res, err := sandboxes.Run(ctx, opts, streams)
			logRunNotices(cli.log, res)

			return relay(err)
		})
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)
	addCreateFlags(flags, &opts.CreateOptions)
	flags.BoolVarP(&interactive, "interactive", "i", false, "attach stdin")
	addCommandFlags(flags, &opts.CommandOptions)
	// An ordinary string flag: a boolean-style --keep would turn
	// "run --keep failure make" into running a command named failure.
	flags.StringVar(&opts.Keep, "keep", sandbox.DefaultKeep, "keep the sandbox, stopped: never, failure, or always")

	return cmd
}

// logRunNotices tells the user what happened to run's sandbox when it
// needs their attention.
func logRunNotices(logger *log.Logger, res sandbox.RunResult) {
	remove := "runspace delete " + res.Name
	inspect := "runspace exec -t " + res.Name + " bash"

	// One case applies: a kept sandbox that could not be stopped is not
	// stopped, so it must not also be reported as kept, stopped.
	switch {
	case res.CleanupErr != nil && res.Kept:
		logger.Warn("Could not stop the kept sandbox; its idle timeout stops it",
			"name", res.Name, "err", res.CleanupErr, "inspect", inspect, "remove", remove)
	case res.CleanupErr != nil:
		logger.Warn("Could not delete the sandbox", "name", res.Name, "err", res.CleanupErr, "remove", remove)
	case res.Kept:
		logger.Info("Kept the sandbox, stopped; it is deleted once its retention ends",
			"name", res.Name, "retention", sandbox.FormatDuration(res.Retention),
			"inspect", inspect, "remove", remove)
	}
}
