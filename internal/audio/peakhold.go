package audio

import (
	"sync"
	"time"
)

// DefaultPeakHoldDuration is the default duration that peak values are held before decaying.
const DefaultPeakHoldDuration = 3000 * time.Millisecond

// PeakHolder tracks peak-hold state for VU meters.
// It is safe for concurrent use.
type PeakHolder struct {
	mu            sync.Mutex
	heldPeakL     float64
	heldPeakR     float64
	peakHoldTimeL time.Time
	peakHoldTimeR time.Time
	holdDuration  time.Duration
}

// NewPeakHolder creates a new peak holder initialized to minimum levels with default duration.
func NewPeakHolder() *PeakHolder {
	return &PeakHolder{
		heldPeakL:    MinDB,
		heldPeakR:    MinDB,
		holdDuration: DefaultPeakHoldDuration,
	}
}

// Update updates the peak hold state with new peak values and returns the held peaks.
func (p *PeakHolder) Update(peakL, peakR float64, now time.Time) (heldL, heldR float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peakL >= p.heldPeakL || now.Sub(p.peakHoldTimeL) > p.holdDuration {
		p.heldPeakL = peakL
		p.peakHoldTimeL = now
	}
	if peakR >= p.heldPeakR || now.Sub(p.peakHoldTimeR) > p.holdDuration {
		p.heldPeakR = peakR
		p.peakHoldTimeR = now
	}
	return p.heldPeakL, p.heldPeakR
}

// SetHoldDuration updates the peak hold duration.
func (p *PeakHolder) SetHoldDuration(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holdDuration = d
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
