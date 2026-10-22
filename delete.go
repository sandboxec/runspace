//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

func newDeleteCmd(cli *app) *cobra.Command {
	var opts sandbox.DeleteOptions

	cmd := new(cobra.Command)
	cmd.Use = "delete [flags] NAME... | --all"
	cmd.Short = "Delete sandboxes"
	cmd.Long = `Delete the named sandboxes, or with --all every sandbox runspace created for
this identity. Codespaces whose display name is not runspace are never
touched.`
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts.Names = args

		// The same check as MCP's delete, so the message matches.
		err := opts.Validate()
		if err != nil {
			return relay(err)
		}

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			_, err := sandboxes.Delete(ctx, opts)

			return relay(err)
		})
	}
	cmd.Flags().BoolVar(&opts.All, "all", false, "delete every sandbox runspace created")

	return cmd
}
