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
		ID: "party1", HostUserID: "host1", ItemID: "item123",
		DurationTicks: 72000000000, CreatedAt: now, Status: PartyStatusCreated,
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
