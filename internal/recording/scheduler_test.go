package recording

import (
	"sync"
	"testing"
)

// TestStartIsIdempotent verifies source retries do not duplicate schedulers.
func TestStartIsIdempotent(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	m.mu.RLock()
	cleanup1, hourly1 := m.cleanupStopCh, m.hourlyRetryStopCh
	m.mu.RUnlock()

	if err := m.Start(); err != nil {
		t.Fatalf("redundant Start: %v", err)
	}

	m.mu.RLock()
	cleanup2, hourly2 := m.cleanupStopCh, m.hourlyRetryStopCh
	m.mu.RUnlock()

	if cleanup1 != cleanup2 {
		t.Error("redundant Start recreated cleanupStopCh; a duplicate cleanup scheduler was spawned")
	}
	if hourly1 != hourly2 {
		t.Error("redundant Start recreated hourlyRetryStopCh; a duplicate hourly retry scheduler was spawned")
	}
}

// TestStartStopConcurrent verifies schedulers do not race Stop's channel reset.
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

	// Tear down any schedulers left running by a trailing Start.
	_ = m.Stop()
}

// TestStopBeforeStartIsNoop verifies nil scheduler channels are safe.
func TestStopBeforeStartIsNoop(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

// TestStartStopCycle verifies repeated lifecycle cycles stay idempotent.
func TestStartStopCycle(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if err := m.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		_ = m.Start() // idempotent
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		_ = m.Stop() // idempotent
	}
}
