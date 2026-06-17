package silencedump

import (
	"sync"
	"testing"
)

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
	m.Stop()
}

func TestStopBeforeStartIsNoop(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	m.Stop() // Must not panic on the nil stop channel.
}

func TestStartStopCycle(t *testing.T) {
	m := NewManager("", 0, false, 0, nil)
	for range 5 {
		m.Start()
		m.Start() // Confirms Start is idempotent.
		m.Stop()
		m.Stop() // Confirms Stop is idempotent.
	}
}
