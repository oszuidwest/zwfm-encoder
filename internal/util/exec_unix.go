//go:build !windows

package util

import "os/exec"

// HideConsole is a no-op on Unix — there is no console window concept.
func HideConsole(_ *exec.Cmd) {}
