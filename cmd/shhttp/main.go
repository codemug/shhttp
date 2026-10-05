// Command shhttp is the command-line client for an shhttp server.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/codemug/shhttp/v2/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	// Interrupts are forwarded to the remote session instead of stopping
	// this process.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, cli.ForwardedSignals()...)
	os.Exit(cli.Main(context.Background(), os.Args[1:], cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Signals: sigs,
	}))
}
