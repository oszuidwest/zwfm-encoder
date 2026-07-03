package util

// OfferDropOldest enqueues buf on ch without ever blocking the producer: when
// the queue is full it evicts one stale chunk and retries the send once. It
// reports whether a stale chunk was evicted. A closed done channel
// short-circuits the initial send during shutdown; nil disables that case.
//
// Queues are expected to have a single producer. Concurrent producers stay
// memory-safe, but eviction accounting may be lossy under contention.
func OfferDropOldest(ch chan []byte, buf []byte, done <-chan struct{}) (evicted bool) {
	select {
	case ch <- buf:
		return false
	case <-done:
		return false
	default:
	}

	select {
	case <-ch:
		evicted = true
	default:
	}

	select {
	case ch <- buf:
	default:
	}
	return evicted
}
