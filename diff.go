//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

func newDiffCmd(cli *app) *cobra.Command {
	var opts sandbox.DiffOptions

	cmd := new(cobra.Command)
	cmd.Use = "diff [flags] NAME"
	cmd.Short = "Print the sandbox's changes as a patch"
	cmd.Long = `Print the patch from the sandbox's base commit to its working tree, covering
commits, uncommitted edits, and new files. With -o, the patch is written to a
file atomically, and only when the diff succeeds.`
	cmd.Args = cobra.ExactArgs(1)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts.Name = args[0]

		err := opts.Validate()
		if err != nil {
			return relay(err)
		}

		streams := cli.streams(nil)

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			_, err := sandboxes.Diff(ctx, opts, streams)

			return relay(err)
		})
	}
	cmd.Flags().StringVarP(&opts.Output, "output", "o", "", "write the patch to this file")
	addTimeoutFlag(cmd.Flags(), &opts.TimeoutOption, "the diff")

	return cmd
}
