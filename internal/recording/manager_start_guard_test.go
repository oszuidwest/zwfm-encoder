package recording

import "testing"

func TestManagerStartIfActiveCannotReviveEndedSource(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	// Checked under the lock Stop takes, or a start can pass it just before Stop.
	underStopLock := func() bool {
		if m.mu.TryLock() {
			m.mu.Unlock()
			t.Error("start predicate ran without the lock Stop takes")
		}
		return false
	}
	if err := m.StartIfActive(underStopLock); err != nil {
		t.Fatal(err)
	}
	if m.IsRunning() {
		_ = m.Stop()
		t.Fatal("late Start revived recorder management after Stop")
	}
}
