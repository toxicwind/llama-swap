package astmatrix

import (
	"sync"
	"time"
)

// PerProviderRateLimiter tracks request rates per provider and implements
// exponential backoff when 429s are received from upstream APIs (notably
// NVIDIA NIM which returns 429 with no body when rate-limited).
type PerProviderRateLimiter struct {
	mu sync.Mutex

	// per-provider state
	requestTimestamps map[string][]time.Time   // rolling window of request timestamps
	backoffUntil      map[string]time.Time     // provider is prohibited until this time
	backoffDuration   map[string]time.Duration // current backoff duration (exponential)
	rateLimitCount    map[string]int           // consecutive 429 count per provider

	// GPU memory pressure (nvidia-smi) — when VRAM is tight, NIM may throttle
	lastGpuCheck time.Time
	gpuFreeMB    int
	gpuTotalMB   int

	// configuration
	windowDuration time.Duration // rolling window (default: 60s)
	maxRequests    int           // max requests per window (default: 60, NIM free tier)
	minBackoff     time.Duration // initial backoff (default: 5s)
	maxBackoff     time.Duration // max backoff (default: 120s)
	backoffFactor  float64       // exponential factor (default: 2.0)
	gpuThrottlePct float64       // GPU mem threshold % that triggers backoff (default: 90)
}

func NewTokenBucket(capacity, rate float64) *TokenBucket {
	return &TokenBucket{
		tokens: capacity, capacity: capacity,
		rate: rate, lastFill: time.Now(),
	}
}

func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock(); defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.tokens = min(tb.capacity, tb.tokens+elapsed*tb.rate)
	tb.lastFill = now
	if tb.tokens >= 1 { tb.tokens--; return true }
	return false
}

// RateLimiter manages per-provider token buckets.
type RateLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*TokenBucket
}

func NewRateLimiter(providers map[string]ProviderCfg) *RateLimiter {
	rl := &RateLimiter{buckets: make(map[string]*TokenBucket)}
	for id, p := range providers {
		rate := 60.0
		if p.FreeTier { rate = 10.0 }
		rl.buckets[id] = NewTokenBucket(rate, rate/60.0)
	}
	return rl
}

func (rl *RateLimiter) Allow(provider string) bool {
	rl.mu.RLock()
	b, ok := rl.buckets[provider]
	rl.mu.RUnlock()
	if !ok { return true }
	return b.Allow()
}
