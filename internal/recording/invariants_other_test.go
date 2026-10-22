//go:build !unix

package recording_test

import (
	"os/exec"
	"time"
)

// crashTests is false: without process groups the harness cannot kill a
// whole tree or see orphaned FFmpeg children exit, so crash scenarios skip.
const crashTests = false

func ownGroup(*exec.Cmd) {}

func killGroup(int) {}

func waitGroupGone(int, time.Duration) bool { return false }
