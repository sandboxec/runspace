//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// listColumnPadding is the blanks between the columns of ls.
const listColumnPadding = 2

func newListCmd(cli *app) *cobra.Command {
	var quiet, asJSON bool

	cmd := new(cobra.Command)
	cmd.Use = "ls [flags]"
	cmd.Short = "List the sandboxes runspace created"
	cmd.Args = cobra.NoArgs
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			list, err := sandboxes.List(ctx, sandbox.ListOptions{})
			if err != nil {
				return relay(err)
			}

			switch {
			case asJSON:
				return relay(json.NewEncoder(cli.stdout).Encode(list))
			case quiet:
				return printNames(cli.stdout, list)
			default:
				return printTable(cli.stdout, list)
			}
		})
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print names only")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.MarkFlagsMutuallyExclusive("quiet", "json")

	return cmd
}

// printNames prints one sandbox name per line.
func printNames(w io.Writer, list []sandbox.Sandbox) error {
	for _, sb := range list {
		_, err := fmt.Fprintln(w, sb.Name)
		if err != nil {
			return relay(err)
		}
	}

	return nil
}

// printTable prints the sandboxes as aligned columns under a header.
func printTable(w io.Writer, list []sandbox.Sandbox) error {
	table := tabwriter.NewWriter(w, 0, 0, listColumnPadding, ' ', 0)

	// A tabwriter buffers; Flush returns the first write error.
	_, _ = fmt.Fprintln(table, "NAME\tSTATE\tREPOSITORY\tMACHINE\tCREATED")

	for _, sb := range list {
		created := sb.CreatedAt.UTC().Format(time.DateTime + " UTC")
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", sb.Name, sb.State, sb.Repository, sb.Machine, created)
	}

	return relay(table.Flush())
}
