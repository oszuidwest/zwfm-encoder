package audio

import (
	"sync"
	"time"
)

// PeakHoldDuration is the duration that peak values are held before decaying.
const PeakHoldDuration = 1500 * time.Millisecond

// PeakHolder tracks peak-hold state for VU meters.
// It is safe for concurrent use.
type PeakHolder struct {
	mu            sync.Mutex
	heldPeakL     float64
	heldPeakR     float64
	peakHoldTimeL time.Time
	peakHoldTimeR time.Time
}

// NewPeakHolder creates a new peak holder initialized to minimum levels.
func NewPeakHolder() *PeakHolder {
	return &PeakHolder{
		heldPeakL: MinDB,
		heldPeakR: MinDB,
	}
}

// Update updates the peak hold state with new peak values and returns the held peaks.
func (p *PeakHolder) Update(peakL, peakR float64, now time.Time) (heldL, heldR float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peakL >= p.heldPeakL || now.Sub(p.peakHoldTimeL) > PeakHoldDuration {
		p.heldPeakL = peakL
		p.peakHoldTimeL = now
	}
	if peakR >= p.heldPeakR || now.Sub(p.peakHoldTimeR) > PeakHoldDuration {
		p.heldPeakR = peakR
		p.peakHoldTimeR = now
	}
	return p.heldPeakL, p.heldPeakR
}

// Reset clears held peak values to minimum levels.
func (p *PeakHolder) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heldPeakL = MinDB
	p.heldPeakR = MinDB
	p.peakHoldTimeL = time.Time{}
	p.peakHoldTimeR = time.Time{}
}
