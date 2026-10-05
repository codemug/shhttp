package cli

import (
	"os"
	"syscall"
)

// Windows has no window-size-change signal.
var winch os.Signal

// ForwardedSignals are the local signals the CLI forwards to sessions.
func ForwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
