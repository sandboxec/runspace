//go:build unix

//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// sizeFD returns the descriptor to read the terminal size from: the terminal
// itself.
func sizeFD(tty *os.File) int { return int(tty.Fd()) }

// watchResize calls resize with the terminal's size after each SIGWINCH.
func watchResize(descriptor int, resize func(width, height int)) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)

	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				width, height, err := term.GetSize(descriptor)
				if err == nil {
					resize(width, height)
				}
			}
		}
	}()

	return func() {
		signal.Stop(sig)
		close(done)
	}
}
