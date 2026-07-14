//go:build windows

package util

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideConsole prevents Windows from allocating a fresh console window for
// this subprocess. Without it, a GUI parent (a `-H windowsgui` build) that
// spawns e.g. FFmpeg gets a briefly-visible console popup per child.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
