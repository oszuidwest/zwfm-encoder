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
	if err := m.StartIfActive(func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	if m.IsRunning() {
		_ = m.Stop()
		t.Fatal("late Start revived recorder management after Stop")
	}
}
