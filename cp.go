//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// copyArgCount is the arguments of cp: SRC and DST.
const copyArgCount = 2

func newCopyCmd(cli *app) *cobra.Command {
	var opts sandbox.CopyOptions

	cmd := new(cobra.Command)
	cmd.Use = "cp [flags] SRC DST"
	cmd.Short = "Copy files between the host and a sandbox"
	cmd.Long = `Copy a file or directory between the host and a sandbox. Exactly one side is
in a sandbox, written NAME:PATH; relative sandbox paths start at
/workspaces/REPO. DST names the result. "-" streams a tar archive:
cp - NAME:DIR extracts stdin into DIR, and cp NAME:SRC - writes SRC to stdout.

Downloads accept only regular files and directories, with sanitized modes, and
never merge into an existing directory.`
	cmd.Args = cobra.ExactArgs(copyArgCount)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts.Src, opts.Dst = args[0], args[1]

		err := opts.Validate()
		if err != nil {
			return relay(err)
		}

		// cp - NAME:DIR extracts stdin, so cp always gets it.
		streams := cli.streams(cli.stdin)

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			_, err := sandboxes.Copy(ctx, opts, streams)

			return relay(err)
		})
	}
	cmd.Flags().BoolVarP(&opts.FollowLink, "follow-link", "L", false, "follow symbolic links in SRC")
	addTimeoutFlag(cmd.Flags(), &opts.TimeoutOption, "the transfer")

	return cmd
}
