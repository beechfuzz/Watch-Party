package dbx

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestMigrationsApplyAndUserRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "user1", "Alice", "encrypted-token", now); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	u, err := s.GetUser(ctx, "user1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.DisplayName != "Alice" || u.EncryptedAccessToken != "encrypted-token" {
		t.Errorf("got %+v", u)
	}

	// Upsert again with new values, confirm update not duplicate insert.
	if err := s.UpsertUser(ctx, "user1", "Alice Renamed", "new-token", now); err != nil {
		t.Fatalf("UpsertUser (update): %v", err)
	}
	u2, err := s.GetUser(ctx, "user1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u2.DisplayName != "Alice Renamed" {
		t.Errorf("DisplayName = %q, want updated value", u2.DisplayName)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetUser(context.Background(), "nonexistent")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "user1", "Alice", "tok", now); err != nil {
		t.Fatal(err)
	}
	sess := Session{
		ID: "sess1", UserID: "user1",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(24 * time.Hour),
		CSRFToken: "csrf-abc",
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession(ctx, "sess1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != "user1" || got.CSRFToken != "csrf-abc" {
		t.Errorf("got %+v", got)
	}

	if err := s.TouchSession(ctx, "sess1", now.Add(time.Minute)); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	got2, _ := s.GetSession(ctx, "sess1")
	if !got2.LastSeenAt.After(got.LastSeenAt) {
		t.Errorf("LastSeenAt did not advance after Touch")
	}

	if err := s.DeleteSession(ctx, "sess1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "sess1"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestPartyAndPlaybackStateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "host1", "Host", "tok", now); err != nil {
		t.Fatal(err)
	}
	party := Party{
		ID: "party1", HostUserID: "host1", Name: "Movie Night",
		CreatedAt: now, Status: PartyStatusCreated,
	}
	if err := s.CreateParty(ctx, party); err != nil {
		t.Fatalf("CreateParty: %v", err)
	}

	if err := s.UpdatePartyStatus(ctx, "party1", PartyStatusActive); err != nil {
		t.Fatalf("UpdatePartyStatus: %v", err)
	}
	got, err := s.GetParty(ctx, "party1")
	if err != nil {
		t.Fatalf("GetParty: %v", err)
	}
	if got.Status != PartyStatusActive {
		t.Errorf("Status = %v, want active", got.Status)
	}

	active, err := s.ListActiveParties(ctx)
	if err != nil {
		t.Fatalf("ListActiveParties: %v", err)
	}
	if len(active) != 1 || active[0].ID != "party1" {
		t.Errorf("ListActiveParties = %+v", active)
	}

	// Member + playback state
	member := PartyMember{
		PartyID: "party1", UserID: "host1", JoinedAt: now,
		ConnectionStatus: ConnConnected,
	}
	if err := s.UpsertMember(ctx, member); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}
	members, err := s.GetMembers(ctx, "party1")
	if err != nil {
		t.Fatalf("GetMembers: %v", err)
	}
	if len(members) != 1 || members[0].UserID != "host1" {
		t.Errorf("GetMembers = %+v", members)
	}

	hostID := "host1"
	state := PlaybackState{
		PartyID: "party1", PositionTicks: 1000, IsPlaying: true,
		SequenceNumber: 1, ServerTimestamp: now,
		UpdatedByUserID: &hostID, UpdatedByClientType: ClientTypeHost,
	}
	if err := s.UpsertPlaybackState(ctx, state); err != nil {
		t.Fatalf("UpsertPlaybackState: %v", err)
	}
	gotState, err := s.GetPlaybackState(ctx, "party1")
	if err != nil {
		t.Fatalf("GetPlaybackState: %v", err)
	}
	if gotState.SequenceNumber != 1 || gotState.PositionTicks != 1000 {
		t.Errorf("gotState = %+v", gotState)
	}

	// Stale write (lower sequence number) must not overwrite newer state.
	staleState := state
	staleState.SequenceNumber = 0
	staleState.PositionTicks = 9999
	if err := s.UpsertPlaybackState(ctx, staleState); err != nil {
		t.Fatalf("UpsertPlaybackState (stale): %v", err)
	}
	gotState2, _ := s.GetPlaybackState(ctx, "party1")
	if gotState2.PositionTicks != 1000 {
		t.Errorf("stale write was applied: PositionTicks = %d, want unchanged 1000", gotState2.PositionTicks)
	}
}

func TestPlaylistItemsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "host1", "Host", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateParty(ctx, Party{ID: "party1", HostUserID: "host1", Name: "Movie Night", CreatedAt: now, Status: PartyStatusActive}); err != nil {
		t.Fatal(err)
	}

	a, err := s.AddPlaylistItem(ctx, "party1", "item-a", 100, "host1", now)
	if err != nil {
		t.Fatalf("AddPlaylistItem: %v", err)
	}
	if a.Position != 0 {
		t.Errorf("first item Position = %d, want 0", a.Position)
	}
	b, err := s.AddPlaylistItem(ctx, "party1", "item-b", 200, "host1", now)
	if err != nil {
		t.Fatalf("AddPlaylistItem: %v", err)
	}
	if b.Position != 1 {
		t.Errorf("second item Position = %d, want 1", b.Position)
	}

	items, err := s.ListPlaylistItems(ctx, "party1")
	if err != nil {
		t.Fatalf("ListPlaylistItems: %v", err)
	}
	if len(items) != 2 || items[0].ItemID != "item-a" || items[1].ItemID != "item-b" {
		t.Fatalf("ListPlaylistItems = %+v, want [item-a, item-b] in order", items)
	}

	// Reorder: swap b before a. This specifically exercises the temporary
	// negative-position staging ReorderPlaylistItems uses -- writing final
	// positions directly, one row at a time, would collide with
	// idx_playlist_items_party_position's unique constraint on a swap.
	if err := s.ReorderPlaylistItems(ctx, "party1", []int64{b.ID, a.ID}); err != nil {
		t.Fatalf("ReorderPlaylistItems: %v", err)
	}
	reordered, err := s.ListPlaylistItems(ctx, "party1")
	if err != nil {
		t.Fatalf("ListPlaylistItems (after reorder): %v", err)
	}
	if len(reordered) != 2 || reordered[0].ItemID != "item-b" || reordered[1].ItemID != "item-a" {
		t.Fatalf("ListPlaylistItems after reorder = %+v, want [item-b, item-a]", reordered)
	}

	if err := s.UpdatePartyCurrentItem(ctx, "party1", &a.ID); err != nil {
		t.Fatalf("UpdatePartyCurrentItem: %v", err)
	}
	got, err := s.GetParty(ctx, "party1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentPlaylistItemID == nil || *got.CurrentPlaylistItemID != a.ID {
		t.Errorf("CurrentPlaylistItemID = %v, want %d", got.CurrentPlaylistItemID, a.ID)
	}

	if err := s.RemovePlaylistItem(ctx, b.ID); err != nil {
		t.Fatalf("RemovePlaylistItem: %v", err)
	}
	remaining, err := s.ListPlaylistItems(ctx, "party1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ItemID != "item-a" {
		t.Errorf("remaining = %+v, want only item-a", remaining)
	}

	if _, err := s.GetPlaylistItem(ctx, b.ID); err != ErrNotFound {
		t.Errorf("GetPlaylistItem(removed) err = %v, want ErrNotFound", err)
	}
}

func TestAddPlaylistItems_SequentialPositionsFromExistingMax(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "host1", "Host", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateParty(ctx, Party{ID: "party1", HostUserID: "host1", Name: "Movie Night", CreatedAt: now, Status: PartyStatusActive}); err != nil {
		t.Fatal(err)
	}

	// One item already queued (position 0) before the batch lands.
	if _, err := s.AddPlaylistItem(ctx, "party1", "item-0", 50, "host1", now); err != nil {
		t.Fatal(err)
	}

	added, err := s.AddPlaylistItems(ctx, "party1", []NewPlaylistItem{
		{ItemID: "item-a", DurationTicks: 100},
		{ItemID: "item-b", DurationTicks: 200},
		{ItemID: "item-c", DurationTicks: 300},
	}, "host1", now)
	if err != nil {
		t.Fatalf("AddPlaylistItems: %v", err)
	}
	if len(added) != 3 {
		t.Fatalf("added = %+v, want 3 rows", added)
	}
	for i, want := range []string{"item-a", "item-b", "item-c"} {
		if added[i].ItemID != want || added[i].Position != i+1 {
			t.Errorf("added[%d] = %+v, want ItemID=%s Position=%d (continuing after the existing item at position 0)", i, added[i], want, i+1)
		}
	}

	items, err := s.ListPlaylistItems(ctx, "party1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("ListPlaylistItems = %+v, want 4 rows total", items)
	}
}

func TestAddPlaylistItems_AtomicOnFailure(t *testing.T) {
	// A batch that fails partway (here: a party_id that doesn't exist,
	// violating the playlist_items foreign key) must leave no rows behind --
	// the whole point of doing this in one transaction instead of N separate
	// inserts.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.UpsertUser(ctx, "host1", "Host", "tok", now); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AddPlaylistItems(ctx, "no-such-party", []NewPlaylistItem{
		{ItemID: "item-a", DurationTicks: 100},
		{ItemID: "item-b", DurationTicks: 200},
	}, "host1", now); err == nil {
		t.Fatal("expected an error inserting playlist items for a nonexistent party (party_id foreign key)")
	}

	items, err := s.ListPlaylistItems(ctx, "no-such-party")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("ListPlaylistItems = %+v, want no rows persisted after a failed batch", items)
	}
}
