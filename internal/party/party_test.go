package party

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

// fakeConn is an in-memory Conn implementation for tests: every Send
// appends to an internal slice, safe for concurrent use since the actor
// goroutine may call Send from onSnapshotTick concurrently with a test
// goroutine reading it.
type fakeConn struct {
	mu        sync.Mutex
	received  []wsproto.Envelope
	closed    bool
	closeCode int
}

func (c *fakeConn) Send(env wsproto.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, env)
}

func (c *fakeConn) Close(code int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeCode = code
}

func (c *fakeConn) last() (wsproto.Envelope, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.received) == 0 {
		return wsproto.Envelope{}, false
	}
	return c.received[len(c.received)-1], true
}

func (c *fakeConn) all() []wsproto.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]wsproto.Envelope, len(c.received))
	copy(out, c.received)
	return out
}

func (c *fakeConn) countOfType(t wsproto.MessageType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.received {
		if e.Type == t {
			n++
		}
	}
	return n
}

func testStore(t *testing.T) *dbx.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("dbx.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return dbx.NewStore(db)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testTuning() Tuning {
	return Tuning{
		SnapshotInterval:  50 * time.Millisecond,
		SoftDriftMS:       300,
		HardDriftMS:       1500,
		MaxRateAdjustment: 0.05,
		HostGracePeriod:   200 * time.Millisecond,
	}
}

// testSettings matches the Party Settings form's own defaults (all three
// checkboxes checked, 5-second delay) — a short delay is overridden per test
// where the countdown itself is what's being exercised.
func testSettings() Settings {
	return Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}
}

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	store := testStore(t)
	h := NewHub(store, testTuning(), testLogger())
	t.Cleanup(func() { h.Shutdown(context.Background()) })
	return h
}

// createPartyWithItem creates a party via the hub, adds one playlist item,
// and seeds it directly as the current item at sequence 0 (bypassing
// transitionCurrent's broadcast/autoplay side effects) — reproducing the
// old single-item design's initial state (idle position 0, paused,
// sequence 0) for tests that only care about play/pause/seek/host-transfer
// behavior on an already-loaded item. Playlist mechanics themselves
// (advance, add/remove/reorder, select) get their own tests.
func createPartyWithItem(t *testing.T, hub *Hub, partyID, itemID string, durationTicks int64, hostUserID string) *Party {
	t.Helper()
	return createPartyWithItemAndSettings(t, hub, partyID, itemID, durationTicks, hostUserID, testSettings())
}

func createPartyWithItemAndSettings(t *testing.T, hub *Hub, partyID, itemID string, durationTicks int64, hostUserID string, settings Settings) *Party {
	t.Helper()
	p, err := hub.CreateParty(context.Background(), partyID, "Test Party", settings, hostUserID)
	if err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	added, err := p.AddPlaylistItem(context.Background(), hostUserID, itemID, durationTicks)
	if err != nil {
		t.Fatalf("AddPlaylistItem: %v", err)
	}
	p.do(func() {
		p.current = &currentItem{PlaylistItemID: added.ID, EmbyItemID: added.ItemID, DurationTicks: added.DurationTicks}
	})
	return p
}

func seedUser(t *testing.T, store *dbx.Store, id, name string) {
	t.Helper()
	if err := store.UpsertUser(context.Background(), id, name, "enc-token", time.Now()); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func decodeAuthoritative(t *testing.T, env wsproto.Envelope) wsproto.AuthoritativeState {
	t.Helper()
	var s wsproto.AuthoritativeState
	if err := json.Unmarshal(env.Payload, &s); err != nil {
		t.Fatalf("decode authoritative state: %v", err)
	}
	return s
}

func decodeSnapshot(t *testing.T, env wsproto.Envelope) wsproto.SnapshotPayload {
	t.Helper()
	var s wsproto.SnapshotPayload
	if err := json.Unmarshal(env.Payload, &s); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return s
}

// --- host authorization ---

func TestHandleControl_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	store := hub.store
	seedUser(t, store, "host", "Host")
	seedUser(t, store, "guest", "Guest")

	p := createPartyWithItem(t, hub, "party1", "item1", 100*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("guest", "Guest")

	guestConn := &fakeConn{}
	p.AttachConn("guest", guestConn)

	err := p.HandleControl("guest", wsproto.MsgPlay, 0)
	if err != ErrNotHost {
		t.Fatalf("err = %v, want ErrNotHost", err)
	}

	env, ok := guestConn.last()
	if !ok || env.Type != wsproto.MsgError {
		t.Fatalf("expected an error message sent to the rejected guest, got %+v (ok=%v)", env, ok)
	}
}

func TestHandleControl_HostAllowed(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 100*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	hostConn := &fakeConn{}
	p.AttachConn("host", hostConn)

	if err := p.HandleControl("host", wsproto.MsgPlay, 5*10_000_000); err != nil {
		t.Fatalf("host play: %v", err)
	}

	env, ok := hostConn.last()
	if !ok || env.Type != wsproto.MsgPlay {
		t.Fatalf("expected a play broadcast, got %+v (ok=%v)", env, ok)
	}
	st := decodeAuthoritative(t, env)
	if st.PositionTicks != 5*10_000_000 || !st.IsPlaying || st.SequenceNumber != 1 {
		t.Errorf("unexpected state: %+v", st)
	}
}

func TestCurrentPositionTicks_ExtrapolatesWhilePlaying(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1_000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	if err := p.HandleControl("host", wsproto.MsgPlay, 5*10_000_000); err != nil {
		t.Fatalf("host play: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	got := p.CurrentPositionTicks()
	// Started at 5s (5*10_000_000 ticks) and playing; some nonzero amount
	// of time has elapsed since, so this must have moved forward from the
	// raw authoritative PositionTicks a snapshot would report -- otherwise
	// a new joiner's transcode would still start at the stale position, not
	// where the party actually is now. See ARCHITECTURE.md §5.4.
	if got <= 5*10_000_000 {
		t.Errorf("CurrentPositionTicks = %d, want > %d (playing, time has elapsed)", got, 5*10_000_000)
	}
	// 50ms of sleep plus scheduling slack shouldn't extrapolate to seconds.
	if got > 5*10_000_000+2*10_000_000 {
		t.Errorf("CurrentPositionTicks = %d, implausibly far ahead of the 5s start plus ~50ms elapsed", got)
	}
}

func TestCurrentPositionTicks_DoesNotAdvanceWhilePaused(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1_000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	if err := p.HandleControl("host", wsproto.MsgPause, 5*10_000_000); err != nil {
		t.Fatalf("host pause: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if got := p.CurrentPositionTicks(); got != 5*10_000_000 {
		t.Errorf("CurrentPositionTicks = %d, want exactly %d while paused (time must not advance it)", got, 5*10_000_000)
	}
}

// --- integration: host connects, participant connects, play/seek/pause propagate ---

func TestIntegration_PlaySeekPausePropagateToParticipant(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")

	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	if _, err := p.Join("host", "Host"); err != nil {
		t.Fatal(err)
	}
	hostConn := &fakeConn{}
	p.AttachConn("host", hostConn)

	if _, err := p.Join("p1", "Participant"); err != nil {
		t.Fatal(err)
	}
	participantConn := &fakeConn{}
	p.AttachConn("p1", participantConn)

	// host plays
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}
	env, ok := participantConn.last()
	if !ok || env.Type != wsproto.MsgPlay {
		t.Fatalf("participant did not receive play broadcast: %+v", env)
	}
	if st := decodeAuthoritative(t, env); !st.IsPlaying {
		t.Errorf("expected IsPlaying=true, got %+v", st)
	}

	// host seeks
	if err := p.HandleControl("host", wsproto.MsgSeek, 200*10_000_000); err != nil {
		t.Fatal(err)
	}
	env, _ = participantConn.last()
	if env.Type != wsproto.MsgSeek {
		t.Fatalf("participant did not receive seek broadcast: %+v", env)
	}
	st := decodeAuthoritative(t, env)
	if st.PositionTicks != 200*10_000_000 {
		t.Errorf("PositionTicks = %d, want %d", st.PositionTicks, 200*10_000_000)
	}
	if !st.IsPlaying {
		t.Errorf("seek should preserve is_playing=true")
	}

	// host pauses
	if err := p.HandleControl("host", wsproto.MsgPause, 210*10_000_000); err != nil {
		t.Fatal(err)
	}
	env, _ = participantConn.last()
	if env.Type != wsproto.MsgPause {
		t.Fatalf("participant did not receive pause broadcast: %+v", env)
	}
	if st := decodeAuthoritative(t, env); st.IsPlaying {
		t.Errorf("expected IsPlaying=false after pause")
	}

	// sequence numbers strictly increasing across all three
	seqs := []int64{}
	for _, e := range participantConn.all() {
		if e.Type == wsproto.MsgPlay || e.Type == wsproto.MsgSeek || e.Type == wsproto.MsgPause {
			seqs = append(seqs, decodeAuthoritative(t, e).SequenceNumber)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("sequence numbers not strictly increasing: %v", seqs)
		}
	}
}

// --- integration: disconnect / reconnect / snapshot ---

func TestIntegration_DisconnectReconnectReceivesCorrectSnapshot(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")

	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	p.AttachConn("host", &fakeConn{})
	_, _ = p.Join("p1", "Participant")
	p.AttachConn("p1", &fakeConn{})

	if err := p.HandleControl("host", wsproto.MsgPlay, 50*10_000_000); err != nil {
		t.Fatal(err)
	}

	p.Disconnect("p1")
	snap := p.Snapshot()
	for _, m := range snap.Members {
		if m.UserID == "p1" && m.ConnectionStatus != string(dbx.ConnDisconnected) {
			t.Errorf("p1 connection status = %s, want disconnected", m.ConnectionStatus)
		}
	}

	// reconnect: Join again should restore connected status and return an
	// up-to-date snapshot reflecting the state that changed while disconnected.
	rejoinSnap, err := p.Join("p1", "Participant")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if !rejoinSnap.IsPlaying || rejoinSnap.PositionTicks != 50*10_000_000 {
		t.Errorf("rejoin snapshot stale: %+v", rejoinSnap)
	}
	found := false
	for _, m := range rejoinSnap.Members {
		if m.UserID == "p1" {
			found = true
			if m.ConnectionStatus != string(dbx.ConnConnected) {
				t.Errorf("after rejoin, connection status = %s, want connected", m.ConnectionStatus)
			}
		}
	}
	if !found {
		t.Error("p1 missing from rejoin snapshot members")
	}
}

// --- rapid-succession command ordering ---

func TestIntegration_RapidSeekThenPlay_FinalStateConsistent(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	if err := p.HandleControl("host", wsproto.MsgSeek, 100*10_000_000); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleControl("host", wsproto.MsgPlay, 100*10_000_000); err != nil {
		t.Fatal(err)
	}

	final := p.Snapshot()
	if !final.IsPlaying || final.PositionTicks != 100*10_000_000 {
		t.Errorf("final state = %+v, want playing at 100s-in-ticks", final)
	}
	if final.SequenceNumber != 2 {
		t.Errorf("SequenceNumber = %d, want 2 (seek then play)", final.SequenceNumber)
	}
}

func TestIntegration_RapidPauseThenSeek_FinalStateConsistent(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	p.AttachConn("host", &fakeConn{})

	_ = p.HandleControl("host", wsproto.MsgPlay, 0)
	_ = p.HandleControl("host", wsproto.MsgPause, 30*10_000_000)
	_ = p.HandleControl("host", wsproto.MsgSeek, 60*10_000_000)

	final := p.Snapshot()
	if final.IsPlaying {
		t.Error("seek after pause should not resume playback")
	}
	if final.PositionTicks != 60*10_000_000 {
		t.Errorf("PositionTicks = %d, want 60s-in-ticks", final.PositionTicks)
	}
	if final.SequenceNumber != 3 {
		t.Errorf("SequenceNumber = %d, want 3", final.SequenceNumber)
	}
}

// --- host grace period ---

func TestHostGracePeriod_ReconnectRetainsHost(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("p1", "Participant")
	p.AttachConn("p1", &fakeConn{})

	p.Disconnect("host")
	time.Sleep(testTuning().HostGracePeriod / 2)
	if _, err := p.Join("host", "Host"); err != nil {
		t.Fatal(err)
	}
	// wait past when grace would have expired had it not been cancelled
	time.Sleep(testTuning().HostGracePeriod)

	if got := p.HostUserID(); got != "host" {
		t.Errorf("HostUserID = %q, want %q (should have retained host after reconnecting within grace period)", got, "host")
	}
}

func TestHostGracePeriod_ExpiresAndTransfersToEarliestJoined(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "early", "Early")
	seedUser(t, hub.store, "late", "Late")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("early", "Early")
	p.AttachConn("early", &fakeConn{})
	time.Sleep(5 * time.Millisecond)
	_, _ = p.Join("late", "Late")
	p.AttachConn("late", &fakeConn{})

	p.Disconnect("host")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.HostUserID() == "early" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.HostUserID(); got != "early" {
		t.Fatalf("HostUserID = %q, want %q after grace period expiry", got, "early")
	}
}

func TestHostGracePeriod_FormerHostDoesNotReclaimOnReconnect(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "successor", "Successor")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("successor", "Successor")
	p.AttachConn("successor", &fakeConn{})

	p.Disconnect("host")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.HostUserID() != "successor" {
		time.Sleep(20 * time.Millisecond)
	}
	if p.HostUserID() != "successor" {
		t.Fatal("host transfer did not happen within deadline")
	}

	// former host reconnects — must NOT reclaim host status automatically
	if _, err := p.Join("host", "Host"); err != nil {
		t.Fatal(err)
	}
	if got := p.HostUserID(); got != "successor" {
		t.Errorf("HostUserID = %q, want %q (former host must not auto-reclaim)", got, "successor")
	}

	// and the former host can no longer issue control commands
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != ErrNotHost {
		t.Errorf("former host HandleControl error = %v, want ErrNotHost", err)
	}
}

// --- explicit leave triggers immediate transfer (no waiting for grace) ---

func TestHostLeave_TransfersImmediately(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("p1", "Participant")
	p.AttachConn("p1", &fakeConn{})

	p.Leave("host")

	if got := p.HostUserID(); got != "p1" {
		t.Errorf("HostUserID = %q, want %q immediately after host leave (no grace period wait)", got, "p1")
	}
}

// --- end of media detection ---

// TestEndOfMedia_EndOfPlaylistGoesIdle covers the single-item case: reaching
// the end of the party's only playlist item, with nothing queued after it,
// transitions the party to idle (paused, no current item) rather than just
// pausing at the last frame — see checkEndOfMedia/transitionCurrent.
func TestEndOfMedia_EndOfPlaylistGoesIdle(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000 // 1 second item
	p := createPartyWithItem(t, hub, "party1", "item1", durationTicks, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}

	// item is 1s long; snapshot tick interval is 50ms in test tuning, so
	// within ~1.2s the server should detect end-of-media on its own,
	// without any client ever sending an "ended" signal (there is no such
	// message type in the protocol at all).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if !snap.IsPlaying && snap.ItemID == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not detect end-of-playlist and go idle within deadline")
}

// TestEndOfMedia_AdvancesToNextPlaylistItem covers the multi-item case:
// reaching the end of the current item enters a (short, 1s) pending
// transition, then advances to the next queued item and keeps playing
// (autoplay_enabled: true) once the countdown fires — rather than stopping.
func TestEndOfMedia_AdvancesToNextPlaylistItem(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000 // 1 second item
	settings := testSettings()
	settings.AutoplayDelaySeconds = 1
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if snap.ItemID == "item2" {
			if !snap.IsPlaying {
				t.Errorf("advanced to item2 but not playing: %+v", snap)
			}
			if snap.PositionTicks != 0 {
				t.Errorf("advanced item should start at position 0, got %d", snap.PositionTicks)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not advance to the next playlist item within deadline")
}

// --- Party Settings: the pending-transition countdown ---

func waitForPendingTransition(t *testing.T, p *Party, deadline time.Time) wsproto.SnapshotPayload {
	t.Helper()
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if snap.PendingTransition != nil {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pending transition never entered within deadline")
	return wsproto.SnapshotPayload{}
}

// TestPendingTransition_EnteredThenFires covers the full happy path: a
// 1-item queue behind the current one, auto_advance on, reaching the end
// enters a pending transition (state clamped to paused-at-duration, snapshot
// carries PendingTransition) rather than advancing immediately, and firing
// lands on the queued item playing (autoplay_enabled: true).
func TestPendingTransition_EnteredThenFires(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoplayDelaySeconds = 1
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}

	snap := waitForPendingTransition(t, p, time.Now().Add(3*time.Second))
	if snap.IsPlaying {
		t.Errorf("state should be clamped to paused while pending, got IsPlaying=true")
	}
	if snap.PositionTicks != durationTicks {
		t.Errorf("PositionTicks = %d, want clamped to duration %d", snap.PositionTicks, durationTicks)
	}
	if snap.ItemID != "item1" {
		t.Errorf("current item should still be item1 during the countdown, got %q", snap.ItemID)
	}
	if !snap.PendingTransition.Autoplay {
		t.Errorf("PendingTransition.Autoplay = false, want true (autoplay_enabled is on)")
	}
	if _, err := time.Parse(time.RFC3339Nano, snap.PendingTransition.Deadline); err != nil {
		t.Errorf("PendingTransition.Deadline not valid RFC3339Nano: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if snap.ItemID == "item2" {
			if !snap.IsPlaying {
				t.Errorf("fired transition should be playing (autoplay_enabled), got paused")
			}
			if snap.PendingTransition != nil {
				t.Errorf("PendingTransition should be cleared once fired")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pending transition never fired within deadline")
}

// TestPendingTransition_AutoplayDisabled_FiresPaused covers
// autoplay_enabled: false — the next item loads but does not start playing.
func TestPendingTransition_AutoplayDisabled_FiresPaused(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoplayDelaySeconds = 1
	settings.AutoplayEnabled = false
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}

	snap := waitForPendingTransition(t, p, time.Now().Add(3*time.Second))
	if snap.PendingTransition.Autoplay {
		t.Errorf("PendingTransition.Autoplay = true, want false (autoplay_enabled is off)")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if snap.ItemID == "item2" {
			if snap.IsPlaying {
				t.Errorf("fired transition should be paused (autoplay_enabled off), got playing")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pending transition never fired within deadline")
}

// TestPendingTransition_AutoAdvanceOff_SkipsCountdownEvenWithQueuedItem
// covers the explicit spec requirement: auto_advance off means end-of-media
// always goes idle immediately, even when a next item IS queued — no
// countdown to nothing, no countdown at all.
func TestPendingTransition_AutoAdvanceOff_SkipsCountdownEvenWithQueuedItem(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoAdvance = false
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if !snap.IsPlaying && snap.ItemID == "" {
			return // idle, straight away -- no PendingTransition ever observed
		}
		if snap.PendingTransition != nil {
			t.Fatal("auto_advance is off, should never enter a pending transition")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not go idle within deadline")
}

// TestPendingTransition_HostCommandCancelsCountdown covers cancellation: a
// host command during the countdown (here, a seek back into the ending
// item) takes precedence and clears the pending transition rather than
// letting it fire underneath the command.
func TestPendingTransition_HostCommandCancelsCountdown(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoplayDelaySeconds = 2
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}
	waitForPendingTransition(t, p, time.Now().Add(3*time.Second))

	if err := p.HandleControl("host", wsproto.MsgSeek, 5*10_000_000); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()
	if snap.PendingTransition != nil {
		t.Fatal("host command should have canceled the pending transition")
	}
	if snap.ItemID != "item1" || snap.PositionTicks != 5*10_000_000 {
		t.Errorf("snap = %+v, want the seek applied to item1", snap)
	}

	// Confirm it really is canceled, not just momentarily cleared: item2
	// must not appear even after the original countdown would have fired.
	time.Sleep(2500 * time.Millisecond)
	if got := p.Snapshot().ItemID; got != "item1" {
		t.Errorf("ItemID = %q after the original countdown's deadline passed, want item1 (canceled, not just delayed)", got)
	}
}

// TestPendingTransition_ResolvesNextAtFireTimeNotEntryTime proves the
// design choice that "what's next" is re-resolved when the deadline fires,
// not fixed when the countdown starts: reordering the playlist during the
// countdown changes what actually plays.
func TestPendingTransition_ResolvesNextAtFireTimeNotEntryTime(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoplayDelaySeconds = 1
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	itemB, err := p.AddPlaylistItem(context.Background(), "host", "item-b", 1000*10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	itemC, err := p.AddPlaylistItem(context.Background(), "host", "item-c", 1000*10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}
	waitForPendingTransition(t, p, time.Now().Add(3*time.Second))

	// item-b was next when the countdown started; remove it mid-countdown.
	// If "next" were captured at entry time, this would have no effect on
	// what fires. It should instead fire on item-c.
	if err := p.RemovePlaylistItem(context.Background(), "host", itemB.ID); err != nil {
		t.Fatal(err)
	}
	_ = itemC

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if snap.ItemID != "" && snap.ItemID != "item1" {
			if snap.ItemID != "item-c" {
				t.Errorf("ItemID = %q, want item-c (item-b was removed mid-countdown)", snap.ItemID)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pending transition never fired within deadline")
}

// TestPendingTransition_LateJoinerGetsSameDeadline covers the plan's
// server-authoritative-countdown requirement: a client joining mid-countdown
// must see the same absolute deadline as one already connected, not a fresh
// full delay -- proving the countdown is driven by shared actor state
// (read fresh by buildSnapshot on every Join), not a per-connection timer.
func TestPendingTransition_LateJoinerGetsSameDeadline(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "late", "Late")
	durationTicks := int64(1) * 10_000_000
	settings := testSettings()
	settings.AutoplayDelaySeconds = 3
	p := createPartyWithItemAndSettings(t, hub, "party1", "item1", durationTicks, "host", settings)
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 1000*10_000_000); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != nil {
		t.Fatal(err)
	}
	firstSnap := waitForPendingTransition(t, p, time.Now().Add(3*time.Second))

	time.Sleep(300 * time.Millisecond) // let some of the countdown elapse
	lateSnap, err := p.Join("late", "Late")
	if err != nil {
		t.Fatal(err)
	}
	if lateSnap.PendingTransition == nil {
		t.Fatal("late joiner's snapshot should include the in-progress pending transition")
	}
	if lateSnap.PendingTransition.Deadline != firstSnap.PendingTransition.Deadline {
		t.Errorf("late joiner's deadline = %q, want the same deadline as the first client (%q), not a restarted delay",
			lateSnap.PendingTransition.Deadline, firstSnap.PendingTransition.Deadline)
	}
}

// TestUpdateSettings_NonHostRejected mirrors the existing host-only
// enforcement pattern for every other party mutation.
func TestUpdateSettings_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	err := p.UpdateSettings(context.Background(), "guest", "New Name", testSettings())
	if err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
}

func TestUpdateSettings_HostAppliesAndBroadcasts(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	newSettings := Settings{AutoAdvance: false, ShowNextDialog: false, AutoplayEnabled: false, AutoplayDelaySeconds: 10}
	if err := p.UpdateSettings(context.Background(), "host", "Renamed Party", newSettings); err != nil {
		t.Fatal(err)
	}

	if p.Name != "Renamed Party" {
		t.Errorf("Name = %q, want %q", p.Name, "Renamed Party")
	}
	snap := p.Snapshot()
	if snap.AutoAdvance || snap.ShowNextDialog || snap.AutoplayEnabled || snap.AutoplayDelaySeconds != 10 {
		t.Errorf("snapshot settings not updated: %+v", snap)
	}
	env, ok := conn.last()
	if !ok || env.Type != wsproto.MsgSnapshot {
		t.Fatalf("expected a snapshot broadcast after settings update, got %+v (ok=%v)", env, ok)
	}
}

// --- playlist mutation: host-only, and the removal/reorder edge cases ---

func TestAddPlaylistItem_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	if _, err := p.AddPlaylistItem(context.Background(), "guest", "item2", 500*10_000_000); err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
}

func TestAddPlaylistItems_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	if _, err := p.AddPlaylistItems(context.Background(), "guest", []ItemToAdd{
		{ItemID: "item2", DurationTicks: 500 * 10_000_000},
	}); err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
	if items, err := hub.store.ListPlaylistItems(context.Background(), "party1"); err != nil || len(items) != 1 {
		t.Errorf("playlist = %+v (err=%v), want unchanged at 1 item (the non-host batch must not have been written)", items, err)
	}
}

func TestAddPlaylistItems_OrderPreserved(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	added, err := p.AddPlaylistItems(context.Background(), "host", []ItemToAdd{
		{ItemID: "item-b", DurationTicks: 200 * 10_000_000},
		{ItemID: "item-a", DurationTicks: 100 * 10_000_000},
		{ItemID: "item-c", DurationTicks: 300 * 10_000_000},
	})
	if err != nil {
		t.Fatalf("AddPlaylistItems: %v", err)
	}
	if len(added) != 3 || added[0].ItemID != "item-b" || added[1].ItemID != "item-a" || added[2].ItemID != "item-c" {
		t.Fatalf("added = %+v, want [item-b, item-a, item-c] preserving the caller's order", added)
	}
	if added[0].Position != 1 || added[1].Position != 2 || added[2].Position != 3 {
		t.Errorf("positions = [%d, %d, %d], want sequential starting after the existing item at position 0", added[0].Position, added[1].Position, added[2].Position)
	}

	// The actor's own in-memory cache (not just the store) must reflect the
	// batch, in the same order -- this is what Snapshot/broadcastPlaylistUpdated
	// serve from, so a mismatch here would only surface as a subtle "playlist
	// looks right after reload but not live" bug.
	var cached []dbx.PlaylistItem
	p.do(func() { cached = append([]dbx.PlaylistItem{}, p.playlist...) })
	if len(cached) != 4 {
		t.Fatalf("actor playlist cache = %+v, want 4 items", cached)
	}
	for i, want := range []string{"item1", "item-b", "item-a", "item-c"} {
		if cached[i].ItemID != want {
			t.Errorf("actor playlist cache[%d] = %q, want %q", i, cached[i].ItemID, want)
		}
	}
}

func TestAddPlaylistItems_SingleBroadcast(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	if _, err := p.AddPlaylistItems(context.Background(), "host", []ItemToAdd{
		{ItemID: "item-a", DurationTicks: 100 * 10_000_000},
		{ItemID: "item-b", DurationTicks: 200 * 10_000_000},
		{ItemID: "item-c", DurationTicks: 300 * 10_000_000},
	}); err != nil {
		t.Fatalf("AddPlaylistItems: %v", err)
	}

	if n := conn.countOfType(wsproto.MsgPlaylistUpdated); n != 1 {
		t.Errorf("playlist_updated broadcasts = %d, want exactly 1 for a 3-item batch (not N)", n)
	}
}

func TestRemovePlaylistItem_CurrentItemRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")

	// The playlist item createPartyWithItem seeded as current -- read it
	// back via the actor's own listing rather than assuming its id.
	snap := p.Snapshot()
	if snap.ItemID != "item1" {
		t.Fatalf("precondition failed: current item = %q, want item1", snap.ItemID)
	}
	var currentID int64
	p.do(func() { currentID = p.current.PlaylistItemID })

	if err := p.RemovePlaylistItem(context.Background(), "host", currentID); err != ErrCannotRemoveCurrentItem {
		t.Errorf("err = %v, want ErrCannotRemoveCurrentItem", err)
	}
}

func TestRemovePlaylistItem_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	added, err := p.AddPlaylistItem(context.Background(), "host", "item2", 500*10_000_000)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.RemovePlaylistItem(context.Background(), "guest", added.ID); err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
}

func TestReorderPlaylist_MismatchedSetRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	if _, err := p.AddPlaylistItem(context.Background(), "host", "item2", 500*10_000_000); err != nil {
		t.Fatal(err)
	}

	if err := p.ReorderPlaylist(context.Background(), "host", []int64{999}); err != ErrInvalidReorder {
		t.Errorf("err = %v, want ErrInvalidReorder", err)
	}
}

func TestSelectCurrentItem_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	added, err := p.AddPlaylistItem(context.Background(), "host", "item2", 500*10_000_000)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.SelectCurrentItem("guest", added.ID); err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
}

func TestSelectCurrentItem_StartsPlayingFromZero(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	added, err := p.AddPlaylistItem(context.Background(), "host", "item2", 500*10_000_000)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.SelectCurrentItem("host", added.ID); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()
	if snap.ItemID != "item2" || !snap.IsPlaying || snap.PositionTicks != 0 {
		t.Errorf("snap = %+v, want item2 playing from position 0", snap)
	}
}

// --- stale/late join gets full snapshot, not replayed history ---

func TestLateJoin_ReceivesFullSnapshotNotHistory(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "late", "Late")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	p.AttachConn("host", &fakeConn{})

	_ = p.HandleControl("host", wsproto.MsgPlay, 0)
	_ = p.HandleControl("host", wsproto.MsgSeek, 10*10_000_000)
	_ = p.HandleControl("host", wsproto.MsgPause, 20*10_000_000)

	snap, err := p.Join("late", "Late")
	if err != nil {
		t.Fatal(err)
	}
	if snap.IsPlaying {
		t.Error("late joiner snapshot should reflect the final paused state")
	}
	if snap.PositionTicks != 20*10_000_000 {
		t.Errorf("PositionTicks = %d, want the final position (20s), not replayed history", snap.PositionTicks)
	}
	if snap.SequenceNumber != 3 {
		t.Errorf("SequenceNumber = %d, want 3", snap.SequenceNumber)
	}
}

// --- host transfer (explicit) ---

func TestExplicitHostTransfer(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("p1", "Participant")
	p.AttachConn("p1", &fakeConn{})

	if err := p.HandleHostTransfer("host", "p1"); err != nil {
		t.Fatal(err)
	}
	if got := p.HostUserID(); got != "p1" {
		t.Errorf("HostUserID = %q, want %q", got, "p1")
	}

	// old host can no longer issue commands, new host can
	if err := p.HandleControl("host", wsproto.MsgPlay, 0); err != ErrNotHost {
		t.Errorf("old host should be rejected, got %v", err)
	}
	if err := p.HandleControl("p1", wsproto.MsgPlay, 0); err != nil {
		t.Errorf("new host should be allowed, got %v", err)
	}
}

func TestExplicitHostTransfer_NonHostRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("p1", "Participant")

	if err := p.HandleHostTransfer("p1", "p1"); err != ErrNotHost {
		t.Errorf("err = %v, want ErrNotHost", err)
	}
}

// --- party lifecycle: ended is terminal ---

func TestPartyEnd_IsTerminalAndClosesConnections(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	if err := hub.EndParty(context.Background(), "party1"); err != nil {
		t.Fatal(err)
	}

	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	if !closed {
		t.Error("connection should be closed when party ends")
	}

	if _, ok := hub.Get("party1"); ok {
		t.Error("ended party should be removed from the hub")
	}

	row, err := hub.store.GetParty(context.Background(), "party1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != dbx.PartyStatusEnded {
		t.Errorf("persisted status = %v, want ended", row.Status)
	}
}

// --- startup recovery ---

func TestHub_RecoverActiveParties(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := dbx.NewStore(db)
	ctx := context.Background()
	seedUser(t, store, "host", "Host")
	seedUser(t, store, "p1", "Participant")

	now := time.Now()
	if err := store.CreateParty(ctx, dbx.Party{
		ID: "party1", HostUserID: "host", Name: "Test Party",
		CreatedAt: now, Status: dbx.PartyStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	item, err := store.AddPlaylistItem(ctx, "party1", "item1", 1000*10_000_000, "host", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePartyCurrentItem(ctx, "party1", &item.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPlaybackState(ctx, dbx.PlaybackState{
		PartyID: "party1", PositionTicks: 42 * 10_000_000, IsPlaying: true,
		SequenceNumber: 7, ServerTimestamp: now, UpdatedByClientType: dbx.ClientTypeHost,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMember(ctx, dbx.PartyMember{
		PartyID: "party1", UserID: "host", JoinedAt: now, ConnectionStatus: dbx.ConnConnected,
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Simulate a restart: reopen the same DB file with a fresh Hub.
	db2, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	store2 := dbx.NewStore(db2)
	hub := NewHub(store2, testTuning(), testLogger())
	defer hub.Shutdown(context.Background())

	if err := hub.RecoverActiveParties(ctx); err != nil {
		t.Fatal(err)
	}

	p, ok := hub.Get("party1")
	if !ok {
		t.Fatal("recovered party not found in hub")
	}
	snap := p.Snapshot()
	if snap.SequenceNumber != 7 || snap.PositionTicks != 42*10_000_000 || !snap.IsPlaying {
		t.Errorf("recovered state = %+v, want seq=7 pos=42s playing=true", snap)
	}
	if snap.DurationTicks != 1000*10_000_000 {
		t.Errorf("DurationTicks not recovered: %+v", snap)
	}
	foundHostDisconnected := false
	for _, m := range snap.Members {
		if m.UserID == "host" {
			foundHostDisconnected = m.ConnectionStatus == string(dbx.ConnDisconnected)
		}
	}
	if !foundHostDisconnected {
		t.Error("recovered members should start as disconnected (zero connected clients) until they reconnect")
	}
}

func TestSweepInactiveParties_EndsPartyIdleLongerThanTimeout(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := dbx.NewStore(db)
	ctx := context.Background()
	seedUser(t, store, "host", "Host")

	staleTime := time.Now().Add(-50 * time.Hour)
	if err := store.CreateParty(ctx, dbx.Party{
		ID: "stale-party", HostUserID: "host", Name: "Stale Party",
		CreatedAt: staleTime, Status: dbx.PartyStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPlaybackState(ctx, dbx.PlaybackState{
		PartyID: "stale-party", PositionTicks: 0, IsPlaying: false,
		SequenceNumber: 0, ServerTimestamp: staleTime, UpdatedByClientType: dbx.ClientTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMember(ctx, dbx.PartyMember{
		PartyID: "stale-party", UserID: "host", JoinedAt: staleTime,
		LastConnectedAt: &staleTime, ConnectionStatus: dbx.ConnDisconnected,
	}); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(store, testTuning(), testLogger())
	defer hub.Shutdown(ctx)
	if err := hub.RecoverActiveParties(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.Get("stale-party"); !ok {
		t.Fatal("party not recovered into hub")
	}

	if ended := hub.SweepInactiveParties(ctx, 48*time.Hour); ended != 1 {
		t.Errorf("SweepInactiveParties returned %d, want 1", ended)
	}
	if _, ok := hub.Get("stale-party"); ok {
		t.Error("stale party still in hub after sweep")
	}
	row, err := store.GetParty(ctx, "stale-party")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != dbx.PartyStatusEnded {
		t.Errorf("party status = %v, want ended", row.Status)
	}
}

func TestSweepInactiveParties_RecentMemberActivityKeepsPartyAlive(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := dbx.NewStore(db)
	ctx := context.Background()
	seedUser(t, store, "host", "Host")

	staleTime := time.Now().Add(-50 * time.Hour)
	recentTime := time.Now().Add(-1 * time.Hour)
	if err := store.CreateParty(ctx, dbx.Party{
		ID: "recently-active-party", HostUserID: "host", Name: "Recently Active Party",
		CreatedAt: staleTime, Status: dbx.PartyStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPlaybackState(ctx, dbx.PlaybackState{
		PartyID: "recently-active-party", PositionTicks: 0, IsPlaying: false,
		SequenceNumber: 0, ServerTimestamp: staleTime, UpdatedByClientType: dbx.ClientTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	// No play/pause/seek in 50h, but the host reconnected 1h ago -- that
	// alone must count as activity and keep the party alive.
	if err := store.UpsertMember(ctx, dbx.PartyMember{
		PartyID: "recently-active-party", UserID: "host", JoinedAt: staleTime,
		LastConnectedAt: &recentTime, ConnectionStatus: dbx.ConnDisconnected,
	}); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(store, testTuning(), testLogger())
	defer hub.Shutdown(ctx)
	if err := hub.RecoverActiveParties(ctx); err != nil {
		t.Fatal(err)
	}

	if ended := hub.SweepInactiveParties(ctx, 48*time.Hour); ended != 0 {
		t.Errorf("SweepInactiveParties returned %d, want 0 (recent member reconnect should count as activity)", ended)
	}
	if _, ok := hub.Get("recently-active-party"); !ok {
		t.Error("party wrongly removed from hub")
	}
}
