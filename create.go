//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

func newCreateCmd(cli *app) *cobra.Command {
	var (
		opts   sandbox.CreateOptions
		asJSON bool
	)

	cmd := new(cobra.Command)
	cmd.Use = "create [-R OWNER/REPO] [flags]"
	cmd.Short = "Create a sandbox and wait until it is ready"
	cmd.Long = `Create a sandbox from a repository and wait until it is ready: available,
reachable over SSH, with HEAD checked out. The base commit is recorded in the
sandbox for diff. A failed or interrupted create deletes the sandbox.

Without -R, OWNER/REPO comes from the git remote origin of the current
directory.`
	cmd.Args = cobra.NoArgs
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var err error

		opts.Repo, err = cli.repo(opts.Repo)
		if err != nil {
			return err
		}

		err = opts.Validate()
		if err != nil {
			return relay(err)
		}

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			res, err := sandboxes.Create(ctx, opts, cli.progress())
			if err != nil {
				return relay(err)
			}

			if asJSON {
				return relay(json.NewEncoder(cli.stdout).Encode(res))
			}

			_, err = fmt.Fprintln(cli.stdout, res.Name)

			return relay(err)
		})
	}
	addCreateFlags(cmd.Flags(), &opts)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print name, repository, workspace, and base as JSON")

	return cmd
}
