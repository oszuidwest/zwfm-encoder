package silencedump

import "testing"

func TestManagerStartIfActiveCannotReviveEndedSource(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	m.Start()
	m.Stop()
	// Checked under the lock Stop takes, or a start can pass it just before Stop.
	m.StartIfActive(func() bool {
		if m.mu.TryLock() {
			m.mu.Unlock()
			t.Error("start predicate ran without the lock Stop takes")
		}
		return false
	})
	if m.IsRunning() {
		m.Stop()
		t.Fatal("late Start revived dump management after Stop")
	}
}
