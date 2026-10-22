//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/mcpserver"
)

func newMCPCmd(cli *app) *cobra.Command {
	cmd := new(cobra.Command)
	cmd.Use = "mcp"
	cmd.Short = "Serve the runspace tools over stdio MCP"
	cmd.Long = `Serve the seven runspace commands as MCP tools over stdin and stdout, for AI
agents. The harness starts and stops the server. Stdin EOF closes every
connection and leaves every sandbox as it is. The server logs to stderr, or
to the file that RUNSPACE_LOG_OUTPUT names, never to stdout.

The tools carry the same authority as the CLI. Bound the agent with a
separate local user, a dedicated GitHub identity, and a Codespaces budget.`
	cmd.Args = cobra.NoArgs
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		sandboxes, err := cli.newSandboxes(cli.token, true)
		if err != nil {
			return err
		}

		return relay(mcpserver.Serve(cmd.Context(), sandboxes, cli.log, version(), cli.stdin, cli.stdout))
	}

	return cmd
}
