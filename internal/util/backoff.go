package util

import (
	"sync"
	"time"
)

// Backoff is an exponential backoff calculator.
// It is safe for concurrent use.
type Backoff struct {
	mu       sync.Mutex
	current  time.Duration
	initial  time.Duration
	maxDelay time.Duration
	factor   float64
}

// NewBackoff returns a new Backoff with the given initial and maximum delays.
func NewBackoff(initial, maxDelay time.Duration) *Backoff {
	return &Backoff{
		current:  initial,
		initial:  initial,
		maxDelay: maxDelay,
		factor:   2.0,
	}
}

// Next returns the current delay and advances to the next value.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.current
	b.current = min(time.Duration(float64(b.current)*b.factor), b.maxDelay)
	return current
}

// Current returns the current delay without advancing.
func (b *Backoff) Current() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current
}

// Reset sets the backoff back to the initial delay.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = b.initial
}
