package util

import "time"

// Backoff is an exponential backoff calculator.
type Backoff struct {
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
	current := b.current
	b.current = min(time.Duration(float64(b.current)*b.factor), b.maxDelay)
	return current
}

// Current returns the current delay without advancing.
func (b *Backoff) Current() time.Duration {
	return b.current
}

// Reset sets the backoff back to the given initial delay.
func (b *Backoff) Reset(initial time.Duration) {
	b.current = initial
	b.initial = initial
}
