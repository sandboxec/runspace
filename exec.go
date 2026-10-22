//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"

	"github.com/spf13/cobra"

	"go.sandbox.ec/runspace/internal/sandbox"
)

// execMinArgs is the arguments exec needs: NAME and CMD.
const execMinArgs = 2

func newExecCmd(cli *app) *cobra.Command {
	var (
		opts             sandbox.ExecOptions
		interactive, tty bool
	)

	cmd := new(cobra.Command)
	cmd.Use = "exec [flags] NAME CMD [ARG...]"
	cmd.Short = "Run a command in a sandbox"
	cmd.Long = `Run a command in a sandbox. Flags come before NAME; everything after NAME
belongs to the command. The exit code is the command's, or 124 when the
timeout expired, 125 when runspace failed, or 130 when interrupted.`
	cmd.Args = cobra.MinimumNArgs(execMinArgs)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts.Name, opts.Argv = args[0], args[1:]

		streams, err := cli.execStreams(interactive, tty)
		if err != nil {
			return err
		}

		err = opts.Validate()
		if err != nil {
			return relay(err)
		}

		return cli.withSandboxes(cmd.Context(), func(ctx context.Context, sandboxes *sandbox.Sandboxes) error {
			return relay(sandboxes.Exec(ctx, opts, streams))
		})
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)
	flags.BoolVarP(&interactive, "interactive", "i", false, "attach stdin")
	flags.BoolVarP(&tty, "tty", "t", false, "allocate a terminal; implies -i")
	addCommandFlags(flags, &opts.CommandOptions)

	return cmd
}

// execStreams returns the streams for exec. interactive attaches stdin; tty
// also allocates a terminal, which needs stdin to be one, and implies
// interactive.
func (a *app) execStreams(interactive, tty bool) (sandbox.Streams, error) {
	if tty && a.tty == nil {
		return a.streams(nil), errTTYNeedsTerminal
	}

	streams := a.streams(a.stdinIf(interactive || tty))
	if tty {
		streams.TTY = a.tty
	}

	return streams, nil
}
