// Command nightshift is "schedule send" for git: prepare commits, pushes and
// pull requests whenever you like, and have them delivered in the night.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	// The time zone database, so --at "... Europe/Zurich" works on every
	// system, Windows included.
	_ "time/tzdata"

	"github.com/michaelbeutler/nightshift/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
