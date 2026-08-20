package party

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

func decodeChatMessage(t *testing.T, env wsproto.Envelope) wsproto.ChatMessagePayload {
	t.Helper()
	var m wsproto.ChatMessagePayload
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatalf("decode chat message: %v", err)
	}
	return m
}

// --- basic send/broadcast ---

func TestHandleChatSend_BroadcastsToAllConnectedMembers(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	hostConn := &fakeConn{}
	p.AttachConn("host", hostConn)
	_, _ = p.Join("guest", "Guest")
	guestConn := &fakeConn{}
	p.AttachConn("guest", guestConn)

	if err := p.HandleChatSend(context.Background(), "guest", "hello everyone"); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	for name, conn := range map[string]*fakeConn{"host": hostConn, "guest": guestConn} {
		env, ok := conn.last()
		if !ok || env.Type != wsproto.MsgChatMessage {
			t.Fatalf("%s did not receive a chat_message broadcast", name)
		}
		msg := decodeChatMessage(t, env)
		if msg.UserID != "guest" || msg.Body != "hello everyone" {
			t.Errorf("%s got %+v, want sender guest / body %q", name, msg, "hello everyone")
		}
		if msg.ID == 0 {
			t.Errorf("%s: message ID was not assigned", name)
		}
		if msg.SentAt == "" {
			t.Errorf("%s: SentAt was not set", name)
		}
	}
}

// This is the point of the feature: unlike every other mutation in this
// codebase (HandleControl, AddPlaylistItem, etc.), chat is authorized by
// mere membership, not host status.
func TestHandleChatSend_NonHostMemberAllowed(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("guest", "Guest")

	if err := p.HandleChatSend(context.Background(), "guest", "hi"); err != nil {
		t.Fatalf("HandleChatSend from non-host member = %v, want nil (not ErrNotHost)", err)
	}
}

// Defense-in-depth: today the only path into HandleChatSend is through an
// already-Join-validated WebSocket connection (httpapi/ws.go's
// dispatchWSMessage), so this case shouldn't be reachable in production --
// but HandleChatSend must not silently accept a userID that never joined
// this party if it's ever called some other way (directly, as this test
// does, or from a future caller). This is the specific case previously
// flagged as having zero test coverage in either direction: nothing
// asserted it was rejected, and nothing asserted it was allowed either.
func TestHandleChatSend_NeverJoinedRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	// Deliberately never call p.Join for this userID.

	if err := p.HandleChatSend(context.Background(), "stranger", "hi"); err != ErrNotMember {
		t.Fatalf("HandleChatSend from a never-joined userID = %v, want ErrNotMember", err)
	}

	msgs, err := hub.store.ListChatMessages(context.Background(), "party1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("stored messages = %+v, want none -- a rejected send must not be persisted", msgs)
	}
}

func TestHandleChatSend_PersistsToStore(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	if err := p.HandleChatSend(context.Background(), "host", "persisted message"); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	msgs, err := hub.store.ListChatMessages(context.Background(), "party1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Body != "persisted message" || msgs[0].UserID != "host" {
		t.Errorf("stored messages = %+v, want one message from host", msgs)
	}
}

func TestHandleChatSend_PartyEndedRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	// Set p.ended directly rather than going through hub.EndParty: EndParty
	// also stops the actor goroutine, and do() short-circuits to a no-op
	// once stopped (see Party.do's doc comment) -- that would test do()'s
	// own stopped-actor behavior instead of HandleChatSend's own p.ended
	// check, which is the thing this test is actually pinning.
	p.do(func() { p.ended = true })
	if err := p.HandleChatSend(context.Background(), "host", "hello"); err != ErrPartyEnded {
		t.Errorf("HandleChatSend on an ended-but-not-yet-stopped party = %v, want ErrPartyEnded", err)
	}
}

// --- length enforcement ---

func TestHandleChatSend_EmptyRejected(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	for _, body := range []string{"", "   ", "\t\n"} {
		if err := p.HandleChatSend(context.Background(), "host", body); err != ErrChatMessageEmpty {
			t.Errorf("HandleChatSend(%q) = %v, want ErrChatMessageEmpty", body, err)
		}
	}
}

func TestHandleChatSend_TooLongRejectedWithErrorFrame(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	tooLong := strings.Repeat("a", MaxChatMessageLength+1)
	if err := p.HandleChatSend(context.Background(), "host", tooLong); err != ErrChatMessageTooLong {
		t.Fatalf("HandleChatSend = %v, want ErrChatMessageTooLong", err)
	}
	// Unlike a rate-limited send, an over-length one gets an explicit error
	// frame back to the sender -- a compliant client's own maxlength should
	// make this unreachable, so it's worth surfacing when it happens anyway.
	env, ok := conn.last()
	if !ok || env.Type != wsproto.MsgError {
		t.Fatal("sender should receive an error frame for an over-length message")
	}
	var payload wsproto.ErrorPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != wsproto.ErrCodeChatMessageLong {
		t.Errorf("error code = %q, want %q", payload.Code, wsproto.ErrCodeChatMessageLong)
	}
}

func TestHandleChatSend_ExactlyMaxLengthAllowed(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	exactly := strings.Repeat("a", MaxChatMessageLength)
	if err := p.HandleChatSend(context.Background(), "host", exactly); err != nil {
		t.Errorf("HandleChatSend at exactly the limit = %v, want nil", err)
	}
}

// A multi-byte emoji must count as one character against the 300 limit, not
// several bytes -- otherwise the server would reject messages a compliant
// client's own (character-counting) length check already accepted.
func TestHandleChatSend_LengthCountsRunesNotBytes(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	exactly300Emoji := strings.Repeat("👋", MaxChatMessageLength) // 4 bytes each in UTF-8
	if err := p.HandleChatSend(context.Background(), "host", exactly300Emoji); err != nil {
		t.Errorf("HandleChatSend(300 emoji) = %v, want nil (rune count, not byte count, must be used)", err)
	}
}

// --- rate limiting ---

func TestHandleChatSend_RateLimitDropsExcessSilently(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	conn := &fakeConn{}
	p.AttachConn("host", conn)

	for i := 0; i < chatRateLimitCapacity; i++ {
		if err := p.HandleChatSend(context.Background(), "host", "msg"); err != nil {
			t.Fatalf("send %d within burst capacity failed: %v", i+1, err)
		}
	}
	if err := p.HandleChatSend(context.Background(), "host", "one too many"); err != ErrChatRateLimited {
		t.Fatalf("send beyond burst capacity = %v, want ErrChatRateLimited", err)
	}
	// Dropped silently: no error frame for the rate-limited send -- only the
	// chat_message broadcasts for the capacity sends that were accepted.
	if n := conn.countOfType(wsproto.MsgError); n != 0 {
		t.Errorf("error frames sent = %d, want 0 (rate limit drops silently, unlike an over-length message)", n)
	}
	if n := conn.countOfType(wsproto.MsgChatMessage); n != chatRateLimitCapacity {
		t.Errorf("chat_message broadcasts = %d, want exactly %d (the rate-limited send must not be persisted or broadcast)", n, chatRateLimitCapacity)
	}
}

// The gap flagged in review: an earlier design deleted a user's rate-limit
// bucket on Leave "to bound memory," which let a user bypass the sustained
// 2/sec cap for free by leaving and rejoining -- 4 free messages per
// reconnect instead of a real sustained limit. The fix is that the bucket
// is never deleted for the life of the Party actor, keyed by userID alone,
// so a reconnect gets back the *same* bucket with whatever state it already
// had.
func TestChatRateLimit_ReconnectDoesNotResetBucket(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	for i := 0; i < chatRateLimitCapacity; i++ {
		if err := p.HandleChatSend(context.Background(), "host", "msg"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	if err := p.HandleChatSend(context.Background(), "host", "over"); err != ErrChatRateLimited {
		t.Fatalf("send beyond capacity = %v, want ErrChatRateLimited", err)
	}

	// Simulate a reconnect: explicit leave, then rejoin.
	p.Leave("host")
	if _, err := p.Join("host", "Host"); err != nil {
		t.Fatalf("rejoin: %v", err)
	}

	if err := p.HandleChatSend(context.Background(), "host", "immediately after rejoin"); err != ErrChatRateLimited {
		t.Fatalf("send immediately after rejoin = %v, want still ErrChatRateLimited (bucket must not reset on reconnect)", err)
	}

	// After enough real elapsed time for the 2/sec refill to grant at least
	// one token, a send should succeed again -- confirming the bucket is
	// still alive and refilling on schedule, not that sends are now
	// permanently blocked.
	time.Sleep(700 * time.Millisecond)
	if err := p.HandleChatSend(context.Background(), "host", "after refill"); err != nil {
		t.Fatalf("send after refill wait = %v, want nil", err)
	}
}

// Same gap as above, via a dropped connection (Disconnect) rather than an
// explicit Leave -- both update the same member map, but this pins that
// neither path touches chatLimiters.
func TestChatRateLimit_DisconnectReconnectDoesNotResetBucket(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	for i := 0; i < chatRateLimitCapacity; i++ {
		if err := p.HandleChatSend(context.Background(), "host", "msg"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	p.Disconnect("host")
	if _, err := p.Join("host", "Host"); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleChatSend(context.Background(), "host", "after reconnect"); err != ErrChatRateLimited {
		t.Fatalf("send after disconnect+reconnect = %v, want still ErrChatRateLimited", err)
	}
}

// Opening a second tab (same account, same party) must not grant a second
// bucket -- the limiter is keyed by userID, not by connection.
func TestChatRateLimit_SameUserSecondConnectionSharesBucket(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")

	for i := 0; i < chatRateLimitCapacity; i++ {
		if err := p.HandleChatSend(context.Background(), "host", "msg"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	// A second AttachConn for the same userID (a second tab) doesn't touch
	// membership or the rate limiter at all -- sending is still gated by
	// the one shared per-userID bucket.
	p.AttachConn("host", &fakeConn{})
	if err := p.HandleChatSend(context.Background(), "host", "from second tab"); err != ErrChatRateLimited {
		t.Fatalf("send from a second connection, same user, over capacity = %v, want ErrChatRateLimited", err)
	}
}

func TestChatRateLimit_ScopedPerUser(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	seedUser(t, hub.store, "guest", "Guest")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	_, _ = p.Join("guest", "Guest")

	for i := 0; i < chatRateLimitCapacity; i++ {
		if err := p.HandleChatSend(context.Background(), "host", "msg"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	if err := p.HandleChatSend(context.Background(), "host", "over"); err != ErrChatRateLimited {
		t.Fatalf("host send beyond capacity = %v, want ErrChatRateLimited", err)
	}
	// A different user's bucket must be entirely unaffected by host's.
	if err := p.HandleChatSend(context.Background(), "guest", "still fine"); err != nil {
		t.Errorf("guest send = %v, want nil (guest's bucket must be independent of host's)", err)
	}
}

// --- retention ---

func TestPartyEnd_DeletesChatMessages(t *testing.T) {
	hub := newTestHub(t)
	seedUser(t, hub.store, "host", "Host")
	p := createPartyWithItem(t, hub, "party1", "item1", 1000*10_000_000, "host")
	_, _ = p.Join("host", "Host")
	if err := p.HandleChatSend(context.Background(), "host", "goodbye world"); err != nil {
		t.Fatal(err)
	}
	msgs, err := hub.store.ListChatMessages(context.Background(), "party1", 200)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("precondition: expected 1 stored message, got %d (err=%v)", len(msgs), err)
	}

	if err := hub.EndParty(context.Background(), "party1"); err != nil {
		t.Fatal(err)
	}

	msgs, err = hub.store.ListChatMessages(context.Background(), "party1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("chat messages after party end = %+v, want none (retention must wipe history the moment a party ends)", msgs)
	}
}

// The retention rule must be hooked into EVERY path that ends a party, not
// just the explicit "End Party" button. Party.End is reached via
// Hub.EndParty either way, but that's exactly the kind of thing that's easy
// to believe is covered without a test that actually exercises the
// automatic inactivity sweep path specifically -- see ARCHITECTURE.md §8.
func TestSweepInactiveParties_DeletesChatMessages(t *testing.T) {
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
	if _, err := store.AddChatMessage(ctx, "stale-party", "host", "anyone still here?", staleTime); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(store, testTuning(), testLogger())
	defer hub.Shutdown(ctx)
	if err := hub.RecoverActiveParties(ctx); err != nil {
		t.Fatal(err)
	}

	if ended := hub.SweepInactiveParties(ctx, 48*time.Hour); ended != 1 {
		t.Fatalf("SweepInactiveParties returned %d, want 1", ended)
	}

	msgs, err := store.ListChatMessages(ctx, "stale-party", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("chat messages after inactivity sweep = %+v, want none", msgs)
	}
}
