//go:build windows

package util

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW from the Win32 process-creation flags.
// The stdlib syscall package doesn't export this constant on Windows, so
// define it here rather than pulling in golang.org/x/sys/windows for one
// value.
const createNoWindow uint32 = 0x08000000

// hideConsole prevents Windows from allocating a fresh console window for
// this subprocess. Without it, a GUI parent (a `-H windowsgui` build) that
// spawns e.g. FFmpeg gets a briefly-visible console popup per child.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
