package silencedump

import (
	"sync"
	"testing"
)

// TestStartStopConcurrent hammers Start and Stop concurrently to surface the
// data race between the cleanup scheduler reading its stop channel and Stop
// closing and nil-ing the field. It must run clean under -race.
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

// TestStopBeforeStartIsNoop ensures Stop is safe when never started (cleanupStopCh is nil).
func TestStopBeforeStartIsNoop(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	m.Stop() // must not panic on the nil stop channel
}

// TestStartStopCycle ensures repeated cycles neither panic on a double close nor
// leave the running flag stuck (Start stays idempotent).
func TestStartStopCycle(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	for range 5 {
		m.Start()
		m.Start() // idempotent
		m.Stop()
		m.Stop() // idempotent
	}
}
