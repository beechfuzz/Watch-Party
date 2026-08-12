package party

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
)

var ErrUnknownParty = errors.New("party: unknown party id")

// Hub owns the registry of active party actors. It holds no playback state
// itself — each Party actor is the source of truth for its own state — the
// Hub is purely for lookup/creation/removal and startup recovery.
type Hub struct {
	mu      sync.Mutex
	parties map[string]*Party

	store  *dbx.Store
	tuning Tuning
	logger *slog.Logger
	events chan Event
}

func NewHub(store *dbx.Store, tuning Tuning, logger *slog.Logger) *Hub {
	return &Hub{
		parties: make(map[string]*Party),
		store:   store, tuning: tuning, logger: logger,
		events: make(chan Event, 256),
	}
}

// Events exposes lifecycle notifications for consumers outside the sync
// engine (per-user Emby playback reporting subscribes to this).
func (h *Hub) Events() <-chan Event { return h.events }

// CreateParty creates a new party's DB row and in-memory actor.
func (h *Hub) CreateParty(ctx context.Context, partyID, itemID string, durationTicks int64, hostUserID string) (*Party, error) {
	now := time.Now()
	row := dbx.Party{
		ID: partyID, HostUserID: hostUserID, ItemID: itemID,
		DurationTicks: durationTicks, CreatedAt: now, Status: dbx.PartyStatusActive,
	}
	if err := h.store.CreateParty(ctx, row); err != nil {
		return nil, err
	}
	initial := State{
		PositionTicks: 0, IsPlaying: false, SequenceNumber: 0,
		ServerTimestamp: now, UpdatedByClientType: dbx.ClientTypeSystem,
	}
	if err := h.store.UpsertPlaybackState(ctx, dbx.PlaybackState{
		PartyID: partyID, PositionTicks: 0, IsPlaying: false, SequenceNumber: 0,
		ServerTimestamp: now, UpdatedByClientType: dbx.ClientTypeSystem,
	}); err != nil {
		return nil, err
	}

	h.mu.Lock()
	p := newParty(partyID, itemID, durationTicks, hostUserID, initial, h.store, h.tuning, h.logger, h.events)
	h.parties[partyID] = p
	h.mu.Unlock()
	return p, nil
}

// Get returns the running actor for partyID, if any is currently active in
// memory (ended/never-created parties are not present here).
func (h *Hub) Get(partyID string) (*Party, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.parties[partyID]
	return p, ok
}

func (h *Hub) remove(partyID string) {
	h.mu.Lock()
	delete(h.parties, partyID)
	h.mu.Unlock()
}

// EndParty transitions a party to the terminal 'ended' state, closes all
// its connections, and removes it from the hub. Ended is irreversible.
func (h *Hub) EndParty(ctx context.Context, partyID string) error {
	p, ok := h.Get(partyID)
	if !ok {
		return ErrUnknownParty
	}
	if err := p.End(ctx); err != nil {
		return err
	}
	p.stop()
	h.remove(partyID)
	return nil
}

// RecoverActiveParties reconstructs in-memory state for every party left in
// 'active' status at last shutdown (i.e. the process died or was restarted
// without a clean end-of-party), with zero connected clients — participants
// reconnect and receive a fresh authoritative snapshot rather than the
// party being silently orphaned.
func (h *Hub) RecoverActiveParties(ctx context.Context) error {
	active, err := h.store.ListActiveParties(ctx)
	if err != nil {
		return err
	}
	for _, row := range active {
		initial := State{ServerTimestamp: time.Now(), UpdatedByClientType: dbx.ClientTypeSystem}
		if st, err := h.store.GetPlaybackState(ctx, row.ID); err == nil {
			initial = State{
				PositionTicks: st.PositionTicks, IsPlaying: st.IsPlaying, SequenceNumber: st.SequenceNumber,
				ServerTimestamp: st.ServerTimestamp, UpdatedByUserID: st.UpdatedByUserID, UpdatedByClientType: st.UpdatedByClientType,
			}
		} else if !errors.Is(err, dbx.ErrNotFound) {
			return err
		}

		h.mu.Lock()
		p := newParty(row.ID, row.ItemID, row.DurationTicks, row.HostUserID, initial, h.store, h.tuning, h.logger, h.events)
		h.parties[row.ID] = p
		h.mu.Unlock()

		members, err := h.store.GetMembers(ctx, row.ID)
		if err != nil {
			h.logger.Error("recovering party members failed", "party_id", row.ID, "error", err)
			continue
		}
		for _, m := range members {
			if m.ConnectionStatus == dbx.ConnLeft {
				continue
			}
			displayName := m.UserID
			if u, err := h.store.GetUser(ctx, m.UserID); err == nil {
				displayName = u.DisplayName
			}
			mm := m
			name := displayName
			p.do(func() {
				p.members[mm.UserID] = &member{
					UserID: mm.UserID, DisplayName: name, JoinedAt: mm.JoinedAt,
					ConnectionStatus: dbx.ConnDisconnected,
				}
			})
		}
		h.logger.Info("recovered active party", "party_id", row.ID, "members", len(members))
	}
	return nil
}

// Shutdown flushes every active party's in-memory state to SQLite
// synchronously and stops all actor goroutines. Called on SIGTERM before
// the process exits (see cmd/server/main.go).
func (h *Hub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	all := make([]*Party, 0, len(h.parties))
	for _, p := range h.parties {
		all = append(all, p)
	}
	h.mu.Unlock()

	for _, p := range all {
		if err := p.FlushSync(ctx); err != nil {
			h.logger.Error("flush on shutdown failed", "party_id", p.ID, "error", err)
		}
		p.stop()
	}
}
