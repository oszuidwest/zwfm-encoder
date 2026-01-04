package util

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Backoff provides retry delay calculation with exponential growth.
type Backoff struct {
	mu           sync.Mutex
	current      time.Duration
	initial      time.Duration
	maxDelay     time.Duration
	factor       float64
	jitterFactor float64 // 0.0 = no jitter, 0.5 = up to 50% random addition
}

// NewBackoff returns a new Backoff with the given initial and maximum delays.
func NewBackoff(initial, maxDelay time.Duration) *Backoff {
	return &Backoff{
		current:      initial,
		initial:      initial,
		maxDelay:     maxDelay,
		factor:       2.0,
		jitterFactor: 0.5,
	}
}

// Next returns the current delay and advances to the next value.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	delay := b.current
	b.current = min(time.Duration(float64(b.current)*b.factor), b.maxDelay)

	// Add jitter to prevent thundering herd
	if b.jitterFactor > 0 {
		jitter := time.Duration(rand.Int64N(int64(float64(delay) * b.jitterFactor)))
		delay += jitter
	}

	return delay
}

// Current returns the current delay.
func (b *Backoff) Current() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current
}

// Reset restores the delay to its initial value.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = b.initial
}
