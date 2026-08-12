// Package syncalg implements the pure math and decision logic behind
// Watch Party's synchronization protocol: clock-offset/RTT estimation,
// expected-position drift calculation, drift-correction decisions
// (rate-nudge vs. hard-seek vs. return-to-1.0), stale update rejection, and
// deterministic host-transfer selection.
//
// This package is the canonical, unit-tested reference for algorithms that
// are also re-implemented in the browser client (web/static/js/sync.js),
// since only the browser can drive an actual <video> element's playback
// rate — see ARCHITECTURE.md for why the logic is intentionally mirrored
// rather than shared across the Go/JS boundary (there is no build pipeline
// to share code through, per the project's "no bundler" constraint). The
// server also runs this same math for its own debug-level sync diagnostics
// logging.
package syncalg

import "time"

// ClockOffsetEstimate is the result of the clock-sync handshake described in
// the spec: client sends t0, server echoes back t1 (its receive time) and t2
// (its send time), client records t3 (its receive time). All four timestamps
// are the same unit (milliseconds since Unix epoch, per each side's own
// clock) and are not assumed to be monotonic across the wire — see the note
// on RFC3339Nano wall-clock timestamps in ARCHITECTURE.md.
type ClockOffsetEstimate struct {
	RTT    time.Duration
	Offset time.Duration // add to the client's local time to estimate the server's time
}

// EstimateClockOffset implements the standard two-timestamp NTP-style
// estimator:
//
//	RTT           = (t3 - t0) - (t2 - t1)
//	clock_offset  = ((t1 - t0) + (t2 - t3)) / 2
//
// This is meaningfully more accurate than assuming one_way_latency = RTT/2
// alone, without needing NTP-level precision.
func EstimateClockOffset(t0, t1, t2, t3 time.Time) ClockOffsetEstimate {
	rtt := t3.Sub(t0) - t2.Sub(t1)
	offset := (t1.Sub(t0) + t2.Sub(t3)) / 2
	return ClockOffsetEstimate{RTT: rtt, Offset: offset}
}

// AuthoritativeState mirrors the fields carried by every authoritative
// state update in the WS protocol (control-event broadcasts and snapshots).
type AuthoritativeState struct {
	PositionTicks   int64
	IsPlaying       bool
	ServerTimestamp time.Time
	SequenceNumber  int64
}

const TicksPerSecond int64 = 10_000_000 // Emby PositionTicks: 100ns units

// ExpectedPosition computes where playback should be right now, given an
// authoritative state, the current local time, and the estimated clock
// offset (local time + offset ≈ server time). If the party is paused, the
// expected position is simply the authoritative position — time does not
// advance.
func ExpectedPosition(state AuthoritativeState, now time.Time, clockOffset time.Duration) int64 {
	if !state.IsPlaying {
		return state.PositionTicks
	}
	serverNow := now.Add(clockOffset)
	elapsed := serverNow.Sub(state.ServerTimestamp)
	if elapsed < 0 {
		elapsed = 0
	}
	return state.PositionTicks + elapsed.Nanoseconds()/100
}

// DriftAction is the correction a client should apply for a given amount of
// measured drift (actual player position vs. ExpectedPosition).
type DriftAction int

const (
	// DriftNone: within the soft threshold, no correction needed. If a rate
	// adjustment was previously active, it must smoothly return to 1.0
	// rather than lingering off-speed (see RecommendedPlaybackRate below).
	DriftNone DriftAction = iota
	// DriftNudgeRate: outside the soft threshold but within the hard
	// threshold — correct by nudging playback rate rather than seeking, to
	// avoid a visible jump.
	DriftNudgeRate
	// DriftHardSeek: exceeds the hard threshold — a rate nudge would take
	// too long to catch up; seek directly instead.
	DriftHardSeek
)

// DriftThresholds bundles the three configurable sync-tuning knobs.
type DriftThresholds struct {
	SoftDriftMS       int
	HardDriftMS       int
	MaxRateAdjustment float64 // e.g. 0.05 == playback rate may range [0.95, 1.05]
}

// ClassifyDrift compares actual player position to expected position (both
// in PositionTicks) and returns the correction action. driftTicks is
// signed: positive means the player is ahead of where it should be.
func ClassifyDrift(actualPositionTicks, expectedPositionTicks int64, t DriftThresholds) (driftTicks int64, action DriftAction) {
	driftTicks = actualPositionTicks - expectedPositionTicks
	absMS := absTicksToMS(driftTicks)
	switch {
	case absMS > t.HardDriftMS:
		action = DriftHardSeek
	case absMS > t.SoftDriftMS:
		action = DriftNudgeRate
	default:
		action = DriftNone
	}
	return driftTicks, action
}

func absTicksToMS(ticks int64) int {
	if ticks < 0 {
		ticks = -ticks
	}
	return int(ticks * 1000 / TicksPerSecond)
}

// RecommendedPlaybackRate returns the playback rate a client should apply
// for a given signed drift, clamped to [1-maxAdjust, 1+maxAdjust]. A player
// that is behind (negative drift, needs to catch up) plays faster than 1.0;
// a player that is ahead plays slower. Proportional to drift magnitude
// relative to the hard threshold so the correction is gentle near the soft
// threshold and closer to the max near the hard threshold.
func RecommendedPlaybackRate(driftTicks int64, t DriftThresholds) float64 {
	if t.HardDriftMS <= 0 {
		return 1.0
	}
	absMS := absTicksToMS(driftTicks)
	fraction := float64(absMS) / float64(t.HardDriftMS)
	if fraction > 1 {
		fraction = 1
	}
	adjust := t.MaxRateAdjustment * fraction
	if driftTicks > 0 {
		// player is ahead of schedule: slow down
		return 1.0 - adjust
	}
	// player is behind schedule: speed up
	return 1.0 + adjust
}

// Member is the minimal shape SelectNewHost needs to pick a successor.
type Member struct {
	UserID    string
	JoinedAt  time.Time
	Connected bool
}

// SelectNewHost implements the deterministic host-transfer rule: when the
// current host's grace period expires without reconnecting, authority
// passes to the currently-connected participant with the earliest
// JoinedAt. excludeUserID (the outgoing host) is never selected, even if
// still marked connected (e.g. caller is choosing a successor before
// updating the outgoing host's own connection state). Returns ("", false)
// if no eligible participant is connected.
func SelectNewHost(members []Member, excludeUserID string) (string, bool) {
	var best *Member
	for i := range members {
		m := &members[i]
		if m.UserID == excludeUserID || !m.Connected {
			continue
		}
		if best == nil || m.JoinedAt.Before(best.JoinedAt) {
			best = m
		}
	}
	if best == nil {
		return "", false
	}
	return best.UserID, true
}

// HostGraceExpired reports whether the host's disconnect grace period has
// elapsed as of now. Once true, the party actor should run SelectNewHost
// and transfer authority; the former host does not automatically reclaim
// host status by reconnecting afterward (it must be granted again, e.g. via
// an explicit host_transfer from the new host).
func HostGraceExpired(disconnectedAt, now time.Time, gracePeriod time.Duration) bool {
	return now.Sub(disconnectedAt) >= gracePeriod
}

// IsStale reports whether an incoming state update should be rejected
// because it carries a sequence number no newer than the currently-applied
// state. This is the rule that prevents a delayed `seek` from clobbering a
// newer `play`, or a stale snapshot from undoing a fresher one.
func IsStale(current, incoming AuthoritativeState) bool {
	return incoming.SequenceNumber <= current.SequenceNumber
}
