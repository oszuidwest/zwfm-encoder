package silencedump

import "testing"

func TestManagerStartIfActiveCannotReviveEndedSource(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	m.Start()
	m.Stop()
	m.StartIfActive(func() bool { return false })
	if m.IsRunning() {
		m.Stop()
		t.Fatal("late Start revived dump management after Stop")
	}
}
