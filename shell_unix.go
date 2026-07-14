//go:build !windows

package main

import "context"

// runShell blocks until the shutdown signal context is cancelled, then
// returns so main can proceed to shutdown.
func runShell(ctx context.Context, _ *app) {
	<-ctx.Done()
}
