package party

import (
	"testing"
	"time"
)

func TestChatBucket_AllowsBurstUpToCapacity(t *testing.T) {
	b := &chatBucket{}
	now := time.Now()
	for i := 0; i < chatRateLimitCapacity; i++ {
		if !b.allow(now) {
			t.Fatalf("send %d within burst capacity was rejected, want allowed", i+1)
		}
	}
	if b.allow(now) {
		t.Fatal("send beyond burst capacity at the same instant was allowed, want rejected")
	}
}

func TestChatBucket_RefillsAtStatedRate(t *testing.T) {
	b := &chatBucket{}
	now := time.Now()
	for i := 0; i < chatRateLimitCapacity; i++ {
		b.allow(now)
	}
	if b.allow(now) {
		t.Fatal("bucket should be empty immediately after exhausting the burst")
	}
	// Refill is 2 tokens/sec (500ms/token) -- 400ms should be short of one.
	if b.allow(now.Add(400 * time.Millisecond)) {
		t.Fatal("400ms elapsed (< 500ms per token at 2/sec) should not have refilled a token")
	}
	// 600ms total is just past one token's worth.
	if !b.allow(now.Add(600 * time.Millisecond)) {
		t.Fatal("600ms elapsed should have refilled at least one token")
	}
}

func TestChatBucket_NeverExceedsCapacityEvenAfterLongIdle(t *testing.T) {
	b := &chatBucket{}
	now := time.Now()
	b.allow(now) // consumes one token, initializing the bucket at capacity

	// A very long idle gap must not let tokens accumulate past capacity --
	// otherwise a user idle for an hour could unload an unbounded burst.
	later := now.Add(time.Hour)
	allowed := 0
	for i := 0; i < chatRateLimitCapacity+2; i++ {
		if b.allow(later) {
			allowed++
		}
	}
	if allowed != chatRateLimitCapacity {
		t.Errorf("allowed = %d sends after a long idle gap, want exactly capacity (%d), not unbounded", allowed, chatRateLimitCapacity)
	}
}

func TestChatBucket_SustainedRateConvergesToStatedCap(t *testing.T) {
	// Sending as fast as the bucket allows over many seconds should total
	// close to capacity (the initial burst) plus refillPerSec*duration (the
	// steady-state trickle) -- not an unbounded or a much slower rate. Over
	// a short window the fixed burst is a real, non-negligible share of the
	// total (that's the whole point of a burst allowance), so the
	// assertion accounts for it explicitly rather than comparing to the
	// bare 2/sec sustained rate, which only holds in the limit as duration
	// grows.
	b := &chatBucket{}
	start := time.Now()
	const duration = 10 * time.Second
	const step = 50 * time.Millisecond
	sent := 0
	for elapsed := time.Duration(0); elapsed <= duration; elapsed += step {
		if b.allow(start.Add(elapsed)) {
			sent++
		}
	}
	want := chatRateLimitCapacity + int(chatRateLimitRefillPerS*duration.Seconds())
	if sent < want-2 || sent > want+2 {
		t.Errorf("sends over %s = %d, want %d ± 2 (capacity=%d burst + %.1f/sec sustained)", duration, sent, want, chatRateLimitCapacity, chatRateLimitRefillPerS)
	}
}

// allowChatSend is the get-or-create lookup: this pins that it's a genuine
// get-or-create, not an accidental "always construct a fresh bucket and
// assign," which would silently defeat the whole limiter while looking
// correct in a quick manual test (a single message always succeeds against
// a freshly-initialized bucket).
func TestAllowChatSend_ReusesExistingBucketAcrossCalls(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	now := time.Now()
	for i := 0; i < chatRateLimitCapacity; i++ {
		if !p.allowChatSend("host", now) {
			t.Fatalf("call %d within capacity was rejected", i+1)
		}
	}
	if p.allowChatSend("host", now) {
		t.Error("call beyond capacity was allowed -- allowChatSend must reuse the same bucket, not create a fresh one each call")
	}
}
