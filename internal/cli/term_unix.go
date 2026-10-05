//go:build unix

package cli

import (
	"os"
	"syscall"
)

// winch is the window-size-change signal, forwarded as a resize.
var winch os.Signal = syscall.SIGWINCH

// ForwardedSignals are the local signals the CLI forwards to sessions.
func ForwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH}
}
