//go:build !windows

package main

import (
	"context"
	"os/signal"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// runShell blocks until a shutdown signal (SIGINT/SIGTERM) arrives, then
// cancels ctx so main can proceed to shutdown.
func runShell(ctx context.Context, cancel context.CancelFunc, _ *app) {
	sigCtx, stop := signal.NotifyContext(ctx, util.ShutdownSignals()...)
	defer stop()

	<-sigCtx.Done()
	cancel()
}
