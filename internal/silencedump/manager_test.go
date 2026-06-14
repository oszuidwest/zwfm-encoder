package silencedump

import (
	"sync"
	"testing"
)

// TestStartStopConcurrent verifies cleanup scheduling does not race Stop.
func TestStartStopConcurrent(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			m.Start()
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			m.Stop()
		}
	}()
	wg.Wait()

	// Tear down any scheduler left running by a trailing Start.
	m.Stop()
}

// TestStopBeforeStartIsNoop verifies nil cleanupStopCh is safe.
func TestStopBeforeStartIsNoop(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	m.Stop() // must not panic on the nil stop channel
}

// TestStartStopCycle verifies repeated lifecycle cycles stay idempotent.
func TestStartStopCycle(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	for range 5 {
		m.Start()
		m.Start() // idempotent
		m.Stop()
		m.Stop() // idempotent
	}
}
