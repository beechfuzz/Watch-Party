package syncalg

import (
	"testing"
	"time"
)

func TestEstimateClockOffset_NoLatencyNoOffset(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t0 := base
	t1 := base.Add(10 * time.Millisecond)
	t2 := base.Add(10 * time.Millisecond)
	t3 := base.Add(20 * time.Millisecond)

	got := EstimateClockOffset(t0, t1, t2, t3)
	if got.RTT != 20*time.Millisecond {
		t.Errorf("RTT = %v, want 20ms", got.RTT)
	}
	if got.Offset != 0 {
		t.Errorf("Offset = %v, want 0", got.Offset)
	}
}

func TestEstimateClockOffset_ServerClockAhead(t *testing.T) {
	// Server clock is 5s ahead of client clock. RTT is 40ms (20ms each way).
	clientBase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	serverOffset := 5 * time.Second

	t0 := clientBase                                         // client send
	t1 := clientBase.Add(serverOffset + 20*time.Millisecond) // server receive
	t2 := clientBase.Add(serverOffset + 20*time.Millisecond) // server send (instant processing)
	t3 := clientBase.Add(40 * time.Millisecond)              // client receive

	got := EstimateClockOffset(t0, t1, t2, t3)
	if got.RTT != 40*time.Millisecond {
		t.Errorf("RTT = %v, want 40ms", got.RTT)
	}
	// offset should be very close to serverOffset
	diff := got.Offset - serverOffset
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Millisecond {
		t.Errorf("Offset = %v, want ~%v", got.Offset, serverOffset)
	}
}

func TestExpectedPosition_Paused(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	state := AuthoritativeState{
		PositionTicks:   5 * TicksPerSecond,
		IsPlaying:       false,
		ServerTimestamp: now.Add(-3 * time.Second),
	}
	got := ExpectedPosition(state, now, 0)
	if got != 5*TicksPerSecond {
		t.Errorf("ExpectedPosition = %d, want %d (paused state doesn't advance)", got, 5*TicksPerSecond)
	}
}

func TestExpectedPosition_Playing(t *testing.T) {
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := stamp.Add(3 * time.Second)
	state := AuthoritativeState{
		PositionTicks:   10 * TicksPerSecond,
		IsPlaying:       true,
		ServerTimestamp: stamp,
	}
	got := ExpectedPosition(state, now, 0)
	want := 13 * TicksPerSecond
	if got != want {
		t.Errorf("ExpectedPosition = %d, want %d", got, want)
	}
}

func TestExpectedPosition_PlayingWithClockOffset(t *testing.T) {
	// Client's local clock is 2s behind the server. If we don't apply the
	// offset, elapsed time is under-counted by 2s.
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clientNow := stamp.Add(3 * time.Second) // client's local clock reading
	offset := 2 * time.Second               // server is 2s ahead
	state := AuthoritativeState{
		PositionTicks:   0,
		IsPlaying:       true,
		ServerTimestamp: stamp,
	}
	got := ExpectedPosition(state, clientNow, offset)
	want := 5 * TicksPerSecond // (3s local + 2s offset) elapsed on the server's clock
	if got != want {
		t.Errorf("ExpectedPosition = %d, want %d", got, want)
	}
}

func TestExpectedPosition_NegativeElapsedClampedToZero(t *testing.T) {
	// server_timestamp slightly in the future relative to now+offset
	// (e.g. tiny clock skew) must not produce a negative elapsed time.
	stamp := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	now := stamp.Add(-100 * time.Millisecond)
	state := AuthoritativeState{PositionTicks: 20 * TicksPerSecond, IsPlaying: true, ServerTimestamp: stamp}
	got := ExpectedPosition(state, now, 0)
	if got != 20*TicksPerSecond {
		t.Errorf("ExpectedPosition = %d, want %d (negative elapsed clamped)", got, 20*TicksPerSecond)
	}
}

func defaultThresholds() DriftThresholds {
	return DriftThresholds{SoftDriftMS: 300, HardDriftMS: 1500, MaxRateAdjustment: 0.05}
}

func TestClassifyDrift_WithinSoft(t *testing.T) {
	drift, action := ClassifyDrift(10*TicksPerSecond, 10*TicksPerSecond+100000 /*10ms*/, defaultThresholds())
	if action != DriftNone {
		t.Errorf("action = %v, want DriftNone", action)
	}
	if drift != -100000 {
		t.Errorf("drift = %d, want -100000", drift)
	}
}

func TestClassifyDrift_SoftBoundaryExclusive(t *testing.T) {
	// exactly at the soft threshold should NOT trigger correction (only
	// strictly greater does), avoiding oscillation right at the boundary.
	exactlyAtSoft := int64(300) * TicksPerSecond / 1000
	_, action := ClassifyDrift(exactlyAtSoft, 0, defaultThresholds())
	if action != DriftNone {
		t.Errorf("action at exact soft boundary = %v, want DriftNone", action)
	}
}

func TestClassifyDrift_NudgeRange(t *testing.T) {
	driftTicks := int64(600) * TicksPerSecond / 1000 // 600ms
	_, action := ClassifyDrift(driftTicks, 0, defaultThresholds())
	if action != DriftNudgeRate {
		t.Errorf("action = %v, want DriftNudgeRate", action)
	}
}

func TestClassifyDrift_HardSeek(t *testing.T) {
	driftTicks := int64(2000) * TicksPerSecond / 1000 // 2000ms
	_, action := ClassifyDrift(driftTicks, 0, defaultThresholds())
	if action != DriftHardSeek {
		t.Errorf("action = %v, want DriftHardSeek", action)
	}
}

func TestRecommendedPlaybackRate_AheadSlowsDown(t *testing.T) {
	driftTicks := int64(750) * TicksPerSecond / 1000 // ahead by 750ms, halfway to hard(1500)
	rate := RecommendedPlaybackRate(driftTicks, defaultThresholds())
	if rate >= 1.0 {
		t.Errorf("rate = %v, want < 1.0 when ahead of schedule", rate)
	}
	if rate < 0.95-1e-9 {
		t.Errorf("rate = %v, should not exceed max adjustment of 0.05", rate)
	}
}

func TestRecommendedPlaybackRate_BehindSpeedsUp(t *testing.T) {
	driftTicks := -int64(750) * TicksPerSecond / 1000
	rate := RecommendedPlaybackRate(driftTicks, defaultThresholds())
	if rate <= 1.0 {
		t.Errorf("rate = %v, want > 1.0 when behind schedule", rate)
	}
}

func TestRecommendedPlaybackRate_ClampedAtMax(t *testing.T) {
	// drift far beyond the hard threshold should still clamp to max adjustment
	driftTicks := int64(10000) * TicksPerSecond / 1000
	rate := RecommendedPlaybackRate(driftTicks, defaultThresholds())
	want := 1.0 - 0.05
	if diff := rate - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("rate = %v, want %v (clamped)", rate, want)
	}
}

func TestRecommendedPlaybackRate_NoDriftReturnsExactlyOne(t *testing.T) {
	rate := RecommendedPlaybackRate(0, defaultThresholds())
	if rate != 1.0 {
		t.Errorf("rate = %v, want exactly 1.0", rate)
	}
}

func TestIsStale(t *testing.T) {
	current := AuthoritativeState{SequenceNumber: 5}
	cases := []struct {
		name     string
		incoming AuthoritativeState
		want     bool
	}{
		{"older sequence rejected", AuthoritativeState{SequenceNumber: 4}, true},
		{"equal sequence rejected (duplicate)", AuthoritativeState{SequenceNumber: 5}, true},
		{"newer sequence accepted", AuthoritativeState{SequenceNumber: 6}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsStale(current, c.incoming); got != c.want {
				t.Errorf("IsStale = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsStale_DelayedSeekAfterNewerPlay(t *testing.T) {
	// Scenario from the spec: a delayed `seek` (seq 3) arrives after a
	// newer `play` (seq 4) was already applied.
	current := AuthoritativeState{SequenceNumber: 4}
	delayedSeek := AuthoritativeState{SequenceNumber: 3}
	if !IsStale(current, delayedSeek) {
		t.Error("delayed seek with older sequence number should be rejected as stale")
	}
}

func TestSelectNewHost_EarliestJoinedAtWins(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	members := []Member{
		{UserID: "host", JoinedAt: base, Connected: false},
		{UserID: "late-joiner", JoinedAt: base.Add(10 * time.Minute), Connected: true},
		{UserID: "early-joiner", JoinedAt: base.Add(1 * time.Minute), Connected: true},
	}
	got, ok := SelectNewHost(members, "host")
	if !ok {
		t.Fatal("expected a successor to be found")
	}
	if got != "early-joiner" {
		t.Errorf("SelectNewHost = %q, want %q", got, "early-joiner")
	}
}

func TestSelectNewHost_ExcludesDisconnected(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	members := []Member{
		{UserID: "host", JoinedAt: base, Connected: false},
		{UserID: "disconnected-but-earliest", JoinedAt: base.Add(1 * time.Minute), Connected: false},
		{UserID: "connected", JoinedAt: base.Add(2 * time.Minute), Connected: true},
	}
	got, ok := SelectNewHost(members, "host")
	if !ok || got != "connected" {
		t.Errorf("SelectNewHost = (%q, %v), want (\"connected\", true)", got, ok)
	}
}

func TestSelectNewHost_NoneEligible(t *testing.T) {
	members := []Member{{UserID: "host", JoinedAt: time.Now(), Connected: true}}
	_, ok := SelectNewHost(members, "host")
	if ok {
		t.Error("expected no eligible successor when only the outgoing host is connected")
	}
}

func TestSelectNewHost_FormerHostDoesNotReclaim(t *testing.T) {
	// After transfer, the former host reconnecting should not be selected
	// again just because they have the earliest JoinedAt — the caller is
	// expected to keep excluding them (they're no longer "the host" in the
	// party's persistent state, just an ordinary participant), so this test
	// documents that SelectNewHost has no special-casing that would make
	// the former host special; it only respects the exclude parameter the
	// caller supplies.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	members := []Member{
		{UserID: "former-host", JoinedAt: base, Connected: true}, // reconnected
		{UserID: "new-host", JoinedAt: base.Add(1 * time.Minute), Connected: true},
	}
	// Caller does NOT exclude former-host this time (simulating that after
	// transfer, former-host is just a normal member and could legitimately
	// be selected in some *future* transfer) — but critically, the party
	// actor itself must not re-run host selection just because former-host
	// reconnected. That invariant lives in the party actor, not here; this
	// test only pins down that SelectNewHost is a stateless picker with no
	// hidden "original host" preference.
	got, ok := SelectNewHost(members, "")
	if !ok || got != "former-host" {
		t.Errorf("SelectNewHost = (%q, %v), want (\"former-host\", true) since exclude was empty and it has the earliest JoinedAt", got, ok)
	}
}

func TestHostGraceExpired(t *testing.T) {
	disconnectedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	grace := 20 * time.Second

	if HostGraceExpired(disconnectedAt, disconnectedAt.Add(19*time.Second), grace) {
		t.Error("grace period should not be expired at 19s of 20s")
	}
	if !HostGraceExpired(disconnectedAt, disconnectedAt.Add(20*time.Second), grace) {
		t.Error("grace period should be expired at exactly 20s")
	}
	if !HostGraceExpired(disconnectedAt, disconnectedAt.Add(30*time.Second), grace) {
		t.Error("grace period should be expired at 30s")
	}
}
