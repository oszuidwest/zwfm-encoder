package util

import (
	"context"
	"os/exec"
)

// Command is exec.Command with console-hiding applied on Windows, so a GUI
// build (-H windowsgui) never flashes a console window for its subprocesses.
// All subprocess spawns in this codebase must go through Command or
// CommandContext.
//
//nolint:gosec // G204: callers pass internal platform/config commands, never user input
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideConsole(cmd)
	return cmd
}

// CommandContext is exec.CommandContext with console-hiding applied on
// Windows. See Command.
//
//nolint:gosec // G204: callers pass internal platform/config commands, never user input
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	hideConsole(cmd)
	return cmd
}
