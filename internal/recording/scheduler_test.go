package recording

import (
	"sync"
	"testing"
)

func TestStartStopConcurrent(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			_ = m.Start()
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			_ = m.Stop()
		}
	}()
	wg.Wait()
	_ = m.Stop()
}

func TestStartStopCycle(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := m.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		cleanup := m.cleanupStopCh
		if err := m.Start(); err != nil {
			t.Fatalf("second Start: %v", err)
		}
		if cleanup != m.cleanupStopCh {
			t.Fatal("redundant Start spawned another cleanup scheduler")
		}
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := m.Stop(); err != nil {
			t.Fatalf("second Stop: %v", err)
		}
	}
}
