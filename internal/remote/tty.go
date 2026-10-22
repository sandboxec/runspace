//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// ttyBaudRate is the input and output speed requested for the pty.
const ttyBaudRate = 14400

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// startTTY requests a pty sized to tty, puts tty in raw mode, and forwards
// size changes. The returned function stops forwarding and restores tty.
func startTTY(sess *ssh.Session, tty *os.File) (func(), error) {
	inputFD := int(tty.Fd())
	sizeSource := sizeFD(tty)

	width, height, err := term.GetSize(sizeSource)
	if err != nil {
		return nil, fmt.Errorf("get terminal size: %w", err)
	}

	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}

	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: ttyBaudRate, ssh.TTY_OP_OSPEED: ttyBaudRate}

	err = sess.RequestPty(termName, height, width, modes)
	if err != nil {
		return nil, fmt.Errorf("request pty: %w", err)
	}

	state, err := term.MakeRaw(inputFD)
	if err != nil {
		return nil, fmt.Errorf("set terminal raw mode: %w", err)
	}

	stop := watchResize(sizeSource, func(width, height int) {
		_ = sess.WindowChange(height, width)
	})

	return func() {
		stop()

		_ = term.Restore(inputFD, state)
	}, nil
}
