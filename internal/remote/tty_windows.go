//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package remote

import (
	"os"
	"time"

	"golang.org/x/term"
)

// resizePollInterval is how often watchResize polls the terminal size.
const resizePollInterval = 250 * time.Millisecond

// sizeFD returns the descriptor to read the terminal size from. Windows
// reports a console's size only through its output buffer, so this is
// stdout even though raw mode is set on stdin.
func sizeFD(*os.File) int { return int(os.Stdout.Fd()) }

// watchResize polls the terminal size every resizePollInterval, because
// Windows has no SIGWINCH, and calls resize when it changes.
func watchResize(descriptor int, resize func(width, height int)) func() {
	ticker := time.NewTicker(resizePollInterval)
	done := make(chan struct{})

	go func() {
		lastWidth, lastHeight, _ := term.GetSize(descriptor)

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				width, height, err := term.GetSize(descriptor)
				if err != nil || (width == lastWidth && height == lastHeight) {
					continue
				}

				lastWidth, lastHeight = width, height

				resize(width, height)
			}
		}
	}()

	return func() {
		ticker.Stop()
		close(done)
	}
}
