package recording

import (
	"log/slog"

	"golang.org/x/sys/unix"
)

// clockSynchronized reports whether the kernel marks the clock as NTP-synchronized.
// An unreadable state counts as synchronized so the check never blocks recording itself.
func clockSynchronized() bool {
	var timex unix.Timex
	state, err := unix.Adjtimex(&timex)
	if err != nil {
		slog.Warn("cannot read clock synchronization, recording without waiting for it", "error", err)
		return true
	}
	return state != unix.TIME_ERROR && timex.Status&unix.STA_UNSYNC == 0
}
