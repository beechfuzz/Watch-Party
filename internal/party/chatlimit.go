package party

import "time"

// chatRateLimit* tune the per-(party, user) chat send rate limiter: a small
// burst allowance on top of a sustained cap, not a hard 500ms gate --
// "effective 2 messages/second" from the client's own local UX throttle,
// not an instantaneous one enforced identically server-side.
const (
	chatRateLimitCapacity   = 4   // max tokens a bucket can hold (burst allowance)
	chatRateLimitRefillPerS = 2.0 // tokens added per second
)

// chatBucket is a per-user token bucket, one per party actor. Deliberately
// keyed only by userID and never deleted for the lifetime of the Party
// actor -- not on Leave, not on Disconnect, not on rejoin -- so a user
// cannot get a fresh burst allowance by reconnecting, opening a second tab
// (same userID, same bucket), or leaving and rejoining the same party. The
// only way to get a new bucket is to join a *different* party, which is a
// different room with its own separate chat history and audience, not a
// bypass of this one's limit. The whole map this lives in is discarded with
// the Party actor at End(), the same lifecycle boundary chat history itself
// uses.
type chatBucket struct {
	tokens     float64
	lastRefill time.Time
}

// allow reports whether a send at time now is permitted, consuming one
// token if so. Refill is computed lazily from elapsed time since the last
// call rather than a background ticker -- there's nothing to tick when no
// one's sending, and this is exactly as accurate for a bucket that's only
// ever read/written from inside the actor's do()-serialized calls.
func (b *chatBucket) allow(now time.Time) bool {
	if b.lastRefill.IsZero() {
		b.tokens = chatRateLimitCapacity
		b.lastRefill = now
	} else if elapsed := now.Sub(b.lastRefill); elapsed > 0 {
		b.tokens += elapsed.Seconds() * chatRateLimitRefillPerS
		if b.tokens > chatRateLimitCapacity {
			b.tokens = chatRateLimitCapacity
		}
		b.lastRefill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allowChatSend is the get-or-create lookup HandleChatSend uses: reuses an
// existing bucket for userID if one exists (carrying over however many
// tokens it has left and when it last refilled), or creates a fresh
// full-capacity one on this user's first send in this party. Getting this
// wrong -- accidentally replacing rather than reusing the map entry on every
// call -- would silently defeat the whole limiter while looking correct in
// a quick manual test, so it's pinned by
// TestChatRateLimit_ReconnectDoesNotResetBucket rather than trusted from
// reading the diff alone.
func (p *Party) allowChatSend(userID string, now time.Time) bool {
	if p.chatLimiters == nil {
		p.chatLimiters = make(map[string]*chatBucket)
	}
	b, ok := p.chatLimiters[userID]
	if !ok {
		b = &chatBucket{}
		p.chatLimiters[userID] = b
	}
	return b.allow(now)
}
