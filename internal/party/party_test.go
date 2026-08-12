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

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	store := testStore(t)
	h := NewHub(store, testTuning(), testLogger())
	t.Cleanup(func() { h.Shutdown(context.Background()) })
	return h
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

	p, err := hub.CreateParty(context.Background(), "party1", "item1", 100*10_000_000, "host")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("guest", "Guest")

	guestConn := &fakeConn{}
	p.AttachConn("guest", guestConn)

	err = p.HandleControl("guest", wsproto.MsgPlay, 0)
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 100*10_000_000, "host")
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

// --- integration: host connects, participant connects, play/seek/pause propagate ---

func TestIntegration_PlaySeekPausePropagateToParticipant(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "p1", "Participant")

	p, err := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
	if err != nil {
		t.Fatal(err)
	}
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

	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("p1", "Participant")
	p.AttachConn("p1", &fakeConn{})

	p.Leave("host")

	if got := p.HostUserID(); got != "p1" {
		t.Errorf("HostUserID = %q, want %q immediately after host leave (no grace period wait)", got, "p1")
	}
}

// --- end of media detection ---

func TestEndOfMedia_ServerDetectsAndBroadcastsPause(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	durationTicks := int64(1) * 10_000_000 // 1 second item
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", durationTicks, "host")
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
		if !snap.IsPlaying && snap.PositionTicks >= durationTicks {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not detect end-of-media within deadline")
}

// --- stale/late join gets full snapshot, not replayed history ---

func TestLateJoin_ReceivesFullSnapshotNotHistory(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "late", "Late")
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
	p, _ := hub.CreateParty(context.Background(), "party1", "item1", 1000*10_000_000, "host")
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
		ID: "party1", HostUserID: "host", ItemID: "item1",
		DurationTicks: 1000 * 10_000_000, CreatedAt: now, Status: dbx.PartyStatusActive,
	}); err != nil {
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
