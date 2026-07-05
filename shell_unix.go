//go:build !windows

package main

import (
	"context"
	"os/signal"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// runShell blocks until a shutdown signal (SIGINT/SIGTERM) arrives, then
// returns so main can proceed to shutdown.
func runShell(_ *app) {
	ctx, stop := signal.NotifyContext(context.Background(), util.ShutdownSignals()...)
	defer stop()

	<-ctx.Done()
}
