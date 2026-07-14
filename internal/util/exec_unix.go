//go:build !windows

package util

import "os/exec"

// hideConsole is a no-op on Unix - there is no console window concept.
func hideConsole(_ *exec.Cmd) {}
