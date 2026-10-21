//go:build unix

package recording_test

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// crashTests reports whether this platform can run the crash scenarios: they
// need process groups to kill a whole tree and to see when it is gone.
const crashTests = true

// ownGroup puts the child in its own process group, so its FFmpeg children
// can be killed with it or waited for after it alone is killed.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup SIGKILLs every process of the group led by pid.
func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// waitGroupGone reports whether every process of the group led by pid exited
// within timeout.
func waitGroupGone(pid int, timeout time.Duration) bool {
	return waitFor(timeout, func() bool { return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) })
}
