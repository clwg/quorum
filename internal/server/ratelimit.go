package server

import (
	"sync"
	"time"
)

// rateLimiter is a simple token-bucket keyed by an arbitrary string
// (username, user ID, ...). It is safe for concurrent use and prunes idle
// buckets lazily so the map does not grow unbounded.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64 // bucket capacity
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*bucket), rate: perSecond, burst: burst}
}

// allow consumes one token for key, returning false if the bucket is empty.
func (r *rateLimiter) allow(key string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buckets[key]
	if b == nil {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * r.rate
	if b.tokens > r.burst {
		b.tokens = r.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
