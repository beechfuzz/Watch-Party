// Package party implements the server-authoritative playback sync engine:
// one actor goroutine per active party, serializing every state mutation so
// sequence numbers and host authorization are correct by construction. See
// ARCHITECTURE.md §3.
package party

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/syncalg"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

var (
	ErrNotHost                 = errors.New("party: user is not host")
	ErrNotMember               = errors.New("party: user is not a member of this party")
	ErrPartyEnded              = errors.New("party: party has ended")
	ErrUnknownTarget           = errors.New("party: target user is not an eligible connected member")
	ErrPlaylistItemNotFound    = errors.New("party: playlist item not found")
	ErrCannotRemoveCurrentItem = errors.New("party: cannot remove the currently-playing playlist item")
	ErrInvalidReorder          = errors.New("party: reorder must include exactly the current playlist item ids")
)

// Conn is the minimal interface the actor needs to push frames to a
// connected client and to close it. The real implementation wraps a
// nhooyr.io/websocket connection with a per-connection writer goroutine so
// a slow client can never block the party actor; tests use a fake.
type Conn interface {
	Send(env wsproto.Envelope)
	Close(code int, reason string)
}

// State is the in-memory authoritative playback state for one party.
type State struct {
	PositionTicks       int64
	IsPlaying           bool
	SequenceNumber      int64
	ServerTimestamp     time.Time
	UpdatedByUserID     *string
	UpdatedByClientType dbx.ClientType
}

func (s State) toWire() wsproto.AuthoritativeState {
	return wsproto.AuthoritativeState{
		PositionTicks:       s.PositionTicks,
		IsPlaying:           s.IsPlaying,
		ServerTimestamp:     s.ServerTimestamp.UTC().Format(time.RFC3339Nano),
		SequenceNumber:      s.SequenceNumber,
		UpdatedByUserID:     s.UpdatedByUserID,
		UpdatedByClientType: string(s.UpdatedByClientType),
	}
}

func (s State) toAlg() syncalg.AuthoritativeState {
	return syncalg.AuthoritativeState{
		PositionTicks:   s.PositionTicks,
		IsPlaying:       s.IsPlaying,
		ServerTimestamp: s.ServerTimestamp,
		SequenceNumber:  s.SequenceNumber,
	}
}

// member is actor-goroutine-private; only ever touched from inside run().
type member struct {
	UserID           string
	DisplayName      string
	JoinedAt         time.Time
	ConnectionStatus dbx.ConnectionStatus
	conn             Conn
}

// Tuning holds the env-configurable sync knobs (see config.Config).
type Tuning struct {
	SnapshotInterval  time.Duration
	SoftDriftMS       int
	HardDriftMS       int
	MaxRateAdjustment float64
	HostGracePeriod   time.Duration
}

func (t Tuning) drift() syncalg.DriftThresholds {
	return syncalg.DriftThresholds{SoftDriftMS: t.SoftDriftMS, HardDriftMS: t.HardDriftMS, MaxRateAdjustment: t.MaxRateAdjustment}
}

// EventType enumerates lifecycle notifications the hub emits for consumers
// outside the sync engine (namely, per-user Emby playback reporting).
type EventType string

const (
	EventJoined       EventType = "joined"        // a user connected (fresh join or reconnect)
	EventDisconnected EventType = "disconnected"  // WS dropped without explicit leave
	EventLeft         EventType = "left"          // explicit leave
	EventEnded        EventType = "party_ended"   // party transitioned to ended (any reason)
	EventStateChanged EventType = "state_changed" // play/pause/seek/system end-of-media applied
	// EventItemChanged fires whenever the party's current item changes
	// (playlist auto-advance, end-of-playlist idle, or a host explicitly
	// selecting a different item) — party-scoped (UserID empty), like
	// EventEnded. ItemID/PositionTicks/IsPlaying describe the OUTGOING
	// item's final state (IsPlaying always false), not the new one: this is
	// what lets embyreport send an accurate Sessions/Playing/Stopped report
	// for the item that just finished. It deliberately does NOT identify
	// the new item — each client re-derives that from the next snapshot and
	// re-fetches its own playback URL, which is what (re-)registers
	// tracking for the new item (see handlePlaybackURL/RecordPlaySession).
	EventItemChanged EventType = "item_changed"
)

// Event is a best-effort notification; consumers must not assume delivery
// is guaranteed (the channel is bounded and full sends are dropped) since
// nothing in the sync-critical path may block on a slow consumer.
//
// PositionTicks/IsPlaying/ItemID are captured at the moment the event is
// raised rather than left for the consumer to re-derive later by looking
// the party back up in the hub — critical for EventEnded specifically,
// since End() immediately removes the party from the hub afterward
// (see Hub.EndParty), so a consumer that tried to call hub.Get() upon
// receiving this event would always find it already gone.
type Event struct {
	Type          EventType
	PartyID       string
	UserID        string // empty for party-scoped events with no single subject user
	ItemID        string
	PositionTicks int64
	IsPlaying     bool
}

// currentItem is the actor-owned pointer to whatever playlist entry is
// presently loaded — nil means idle (nothing loaded: a freshly-created
// party with an empty playlist, or one whose queue just ran out). Unlike
// the old single-item design, this changes over the party's lifetime, so it
// lives alongside state as actor-owned, do()-serialized data rather than an
// immutable field set once at construction.
type currentItem struct {
	PlaylistItemID int64
	EmbyItemID     string
	DurationTicks  int64
}

// Party is the actor for one party: all fields below the run() goroutine
// line are only ever read/written from inside that goroutine.
type Party struct {
	ID   string
	Name string

	store  *dbx.Store
	tuning Tuning
	logger *slog.Logger
	events chan<- Event

	cmdCh   chan func()
	stopCh  chan struct{}
	stopped chan struct{}

	// actor-owned state
	state              State
	hostUserID         string
	members            map[string]*member
	hostDisconnectedAt *time.Time
	ended              bool
	current            *currentItem
	playlist           []dbx.PlaylistItem // ordered by Position; actor's in-memory cache of playlist_items
}

func newParty(id, name, hostUserID string, initial State, current *currentItem, playlist []dbx.PlaylistItem, store *dbx.Store, tuning Tuning, logger *slog.Logger, events chan<- Event) *Party {
	p := &Party{
		ID: id, Name: name,
		store: store, tuning: tuning, logger: logger, events: events,
		cmdCh: make(chan func()), stopCh: make(chan struct{}), stopped: make(chan struct{}),
		state: initial, hostUserID: hostUserID, members: make(map[string]*member),
		current: current, playlist: playlist,
	}
	go p.run()
	return p
}

// currentEmbyItemID returns the Emby ItemId of whatever's currently loaded,
// or "" when idle — the wire/DB representation of "nothing loaded" is
// simply an empty ItemID, not a separate status field.
func (p *Party) currentEmbyItemID() string {
	if p.current == nil {
		return ""
	}
	return p.current.EmbyItemID
}

func (p *Party) run() {
	defer close(p.stopped)
	snapshotTicker := time.NewTicker(p.tuning.SnapshotInterval)
	defer snapshotTicker.Stop()
	graceTicker := time.NewTicker(time.Second)
	defer graceTicker.Stop()

	for {
		select {
		case fn, ok := <-p.cmdCh:
			if !ok {
				return
			}
			fn()
		case <-snapshotTicker.C:
			p.onSnapshotTick()
		case <-graceTicker.C:
			p.onGraceTick()
		case <-p.stopCh:
			return
		}
	}
}

// do enqueues fn to run serially inside the actor goroutine and blocks
// until it has executed. At this project's scale (≤20 concurrent users)
// synchronous dispatch keeps the logic easy to reason about; see
// ARCHITECTURE.md §3.
func (p *Party) do(fn func()) {
	done := make(chan struct{})
	select {
	case p.cmdCh <- func() { fn(); close(done) }:
		select {
		case <-done:
		case <-p.stopped:
		}
	case <-p.stopped:
	}
}

func (p *Party) emit(evt Event) {
	evt.PartyID = p.ID
	select {
	case p.events <- evt:
	default:
	}
}

func (p *Party) persistStateAsync() {
	st := p.state
	partyID := p.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		row := dbx.PlaybackState{
			PartyID: partyID, PositionTicks: st.PositionTicks, IsPlaying: st.IsPlaying,
			SequenceNumber: st.SequenceNumber, ServerTimestamp: st.ServerTimestamp,
			UpdatedByUserID: st.UpdatedByUserID, UpdatedByClientType: st.UpdatedByClientType,
		}
		if err := p.store.UpsertPlaybackState(ctx, row); err != nil {
			p.logger.Error("persist playback state failed", "party_id", partyID, "error", err)
		}
	}()
}

func (p *Party) connectedMembersForSelection() []syncalg.Member {
	out := make([]syncalg.Member, 0, len(p.members))
	for _, m := range p.members {
		out = append(out, syncalg.Member{
			UserID: m.UserID, JoinedAt: m.JoinedAt, Connected: m.ConnectionStatus == dbx.ConnConnected,
		})
	}
	return out
}

func (p *Party) buildSnapshot() wsproto.SnapshotPayload {
	members := make([]wsproto.MemberInfo, 0, len(p.members))
	for _, m := range p.members {
		members = append(members, wsproto.MemberInfo{
			UserID: m.UserID, DisplayName: m.DisplayName,
			ConnectionStatus: string(m.ConnectionStatus), IsHost: m.UserID == p.hostUserID,
		})
	}
	var durationTicks int64
	if p.current != nil {
		durationTicks = p.current.DurationTicks
	}
	return wsproto.SnapshotPayload{
		AuthoritativeState: p.state.toWire(),
		PartyID:            p.ID,
		ItemID:             p.currentEmbyItemID(),
		DurationTicks:      durationTicks,
		HostUserID:         p.hostUserID,
		Members:            members,
	}
}

func (p *Party) buildPlaylistPayload() wsproto.PlaylistUpdatedPayload {
	items := make([]wsproto.PlaylistItemRef, 0, len(p.playlist))
	for _, it := range p.playlist {
		items = append(items, wsproto.PlaylistItemRef{ID: it.ID, ItemID: it.ItemID, Position: it.Position})
	}
	return wsproto.PlaylistUpdatedPayload{Items: items}
}

func (p *Party) broadcastPlaylistUpdated() {
	payload, err := json.Marshal(p.buildPlaylistPayload())
	if err != nil {
		p.logger.Error("marshal playlist broadcast failed", "party_id", p.ID, "error", err)
		return
	}
	env := wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgPlaylistUpdated, Payload: payload}
	for _, m := range p.members {
		if m.conn != nil {
			m.conn.Send(env)
		}
	}
}

func (p *Party) broadcastSnapshot() {
	payload, err := json.Marshal(p.buildSnapshot())
	if err != nil {
		p.logger.Error("marshal snapshot failed", "party_id", p.ID, "error", err)
		return
	}
	env := wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgSnapshot, Payload: payload}
	for _, m := range p.members {
		if m.conn != nil {
			m.conn.Send(env)
		}
	}
}

func (p *Party) broadcastControl(msgType wsproto.MessageType) {
	payload, err := json.Marshal(p.state.toWire())
	if err != nil {
		p.logger.Error("marshal control broadcast failed", "party_id", p.ID, "error", err)
		return
	}
	env := wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: msgType, Payload: payload}
	for _, m := range p.members {
		if m.conn != nil {
			m.conn.Send(env)
		}
	}
}

func (p *Party) sendError(m *member, code, message string) {
	if m == nil || m.conn == nil {
		return
	}
	payload, _ := json.Marshal(wsproto.ErrorPayload{Code: code, Message: message})
	m.conn.Send(wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgError, Payload: payload})
}

func (p *Party) onSnapshotTick() {
	if p.ended {
		return
	}
	p.checkEndOfMedia()
	if p.ended {
		return
	}
	p.broadcastSnapshot()
	p.persistStateAsync()
}

// checkEndOfMedia is the server-authoritative end-of-media detection: it
// computes the party's *current* expected position (the same math a client
// uses for drift correction) rather than trusting any single participant's
// browser `ended` event, per the spec's explicit requirement that a
// spurious client-side ended event must not unilaterally end playback for
// everyone. Reaching the end now advances to the next playlist item (if
// any) instead of just pausing — see nextPlaylistItemAfter/transitionCurrent.
func (p *Party) checkEndOfMedia() {
	if !p.state.IsPlaying || p.current == nil || p.current.DurationTicks <= 0 {
		return
	}
	expected := syncalg.ExpectedPosition(p.state.toAlg(), time.Now(), 0)
	if expected < p.current.DurationTicks {
		return
	}
	next := p.nextPlaylistItemAfter(p.current.PlaylistItemID)
	if next == nil {
		p.logger.Info("end of playlist reached", "party_id", p.ID)
		p.transitionCurrent(nil, false, nil, dbx.ClientTypeSystem)
		return
	}
	p.logger.Info("advancing to next playlist item", "party_id", p.ID, "item_id", next.ItemID)
	p.transitionCurrent(&currentItem{PlaylistItemID: next.ID, EmbyItemID: next.ItemID, DurationTicks: next.DurationTicks}, true, nil, dbx.ClientTypeSystem)
}

// nextPlaylistItemAfter returns the playlist entry immediately following
// afterPlaylistItemID by Position, or nil if that was the last one (or
// wasn't found at all, e.g. it was concurrently removed).
func (p *Party) nextPlaylistItemAfter(afterPlaylistItemID int64) *dbx.PlaylistItem {
	for i, it := range p.playlist {
		if it.ID == afterPlaylistItemID {
			if i+1 < len(p.playlist) {
				return &p.playlist[i+1]
			}
			return nil
		}
	}
	return nil
}

// transitionCurrent is the single path every "current item changed" case
// goes through: system auto-advance/idle (checkEndOfMedia) and an explicit
// host selection (SelectCurrentItem). It resets playback state to the start
// of whatever's now current (or to idle), bumping the same sequence-number
// counter every other authoritative change uses — so a delayed pre-
// transition snapshot is rejected by clients exactly like a stale seek
// would be, with no separate numbering scheme needed. If there was a
// previous current item, its final live position is captured and emitted
// as EventItemChanged so embyreport can send that item's Stopped report
// (see the EventItemChanged doc comment) before the new item's tracking
// begins (which happens lazily, the same way it does on join: whenever a
// client fetches a playback URL for it).
func (p *Party) transitionCurrent(next *currentItem, playing bool, byUserID *string, clientType dbx.ClientType) {
	var outgoingItemID string
	var outgoingFinalPos int64
	hadPrevious := p.current != nil
	if hadPrevious {
		outgoingItemID = p.current.EmbyItemID
		outgoingFinalPos = syncalg.ExpectedPosition(p.state.toAlg(), time.Now(), 0)
	}

	p.current = next
	p.state = State{
		PositionTicks: 0, IsPlaying: playing,
		SequenceNumber: p.state.SequenceNumber + 1, ServerTimestamp: time.Now(),
		UpdatedByUserID: byUserID, UpdatedByClientType: clientType,
	}

	var newPlaylistItemID *int64
	if next != nil {
		id := next.PlaylistItemID
		newPlaylistItemID = &id
	}
	partyID := p.ID
	store := p.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.UpdatePartyCurrentItem(ctx, partyID, newPlaylistItemID); err != nil {
			p.logger.Error("persist current item failed", "party_id", partyID, "error", err)
		}
	}()
	p.broadcastSnapshot()
	p.persistStateAsync()

	if hadPrevious {
		p.emit(Event{Type: EventItemChanged, ItemID: outgoingItemID, PositionTicks: outgoingFinalPos, IsPlaying: false})
	}
}

func (p *Party) onGraceTick() {
	if p.ended || p.hostDisconnectedAt == nil {
		return
	}
	if !syncalg.HostGraceExpired(*p.hostDisconnectedAt, time.Now(), p.tuning.HostGracePeriod) {
		return
	}
	oldHost := p.hostUserID
	newHost, ok := syncalg.SelectNewHost(p.connectedMembersForSelection(), oldHost)
	p.hostDisconnectedAt = nil
	if !ok {
		p.logger.Warn("host grace period expired with no eligible successor", "party_id", p.ID)
		return
	}
	p.hostUserID = newHost
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.store.UpdatePartyHost(ctx, p.ID, newHost); err != nil {
			p.logger.Error("persist host transfer failed", "party_id", p.ID, "error", err)
		}
	}()
	p.broadcastSnapshot()
	p.logger.Info("host transferred after grace period", "party_id", p.ID, "old_host", oldHost, "new_host", newHost)
}

// --- public actor API: each call is dispatched through do() ---

// Join registers conn as the live connection for userID (fresh join or
// reconnect) and returns the current snapshot to send back. If the
// rejoining user is the current host mid-grace-period, the grace timer is
// cancelled and they keep host status.
func (p *Party) Join(userID, displayName string) (wsproto.SnapshotPayload, error) {
	var snap wsproto.SnapshotPayload
	var outErr error
	p.do(func() {
		if p.ended {
			outErr = ErrPartyEnded
			return
		}
		now := time.Now()
		m, exists := p.members[userID]
		if !exists {
			m = &member{UserID: userID, DisplayName: displayName, JoinedAt: now}
			p.members[userID] = m
		}
		m.ConnectionStatus = dbx.ConnConnected
		m.DisplayName = displayName
		if userID == p.hostUserID {
			p.hostDisconnectedAt = nil
		}
		snap = p.buildSnapshot()
		joinedAt := m.JoinedAt
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.store.UpsertMember(ctx, dbx.PartyMember{
				PartyID: p.ID, UserID: userID, JoinedAt: joinedAt,
				LastConnectedAt: &now, ConnectionStatus: dbx.ConnConnected,
			}); err != nil {
				p.logger.Error("persist member join failed", "party_id", p.ID, "user_id", userID, "error", err)
			}
		}()
		p.emit(Event{Type: EventJoined, UserID: userID, ItemID: p.currentEmbyItemID(), PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
	})
	return snap, outErr
}

// AttachConn sets the live Conn for an already-joined member (join and the
// transport handshake are separate steps: Join records membership/host
// grace logic and returns the snapshot to send, AttachConn is called once
// the caller is ready to receive further broadcasts on that connection).
func (p *Party) AttachConn(userID string, conn Conn) {
	p.do(func() {
		if m, ok := p.members[userID]; ok {
			m.conn = conn
		}
	})
}

// Disconnect marks userID as disconnected (not left) — a dropped
// connection is not a departure. If userID is the host, starts the grace
// period clock.
func (p *Party) Disconnect(userID string) {
	p.do(func() {
		m, ok := p.members[userID]
		if !ok || p.ended {
			return
		}
		m.ConnectionStatus = dbx.ConnDisconnected
		m.conn = nil
		if userID == p.hostUserID {
			now := time.Now()
			p.hostDisconnectedAt = &now
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.store.UpdateMemberConnectionStatus(ctx, p.ID, userID, dbx.ConnDisconnected, nil); err != nil {
				p.logger.Error("persist disconnect failed", "party_id", p.ID, "user_id", userID, "error", err)
			}
		}()
		p.emit(Event{Type: EventDisconnected, UserID: userID, ItemID: p.currentEmbyItemID(), PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
	})
}

// Leave marks userID as having explicitly left. Unlike a disconnect, if the
// leaving user is host, a successor is selected immediately rather than
// waiting out the grace period, since this is a deliberate action, not a
// network blip.
func (p *Party) Leave(userID string) {
	p.do(func() {
		m, ok := p.members[userID]
		if !ok || p.ended {
			return
		}
		m.ConnectionStatus = dbx.ConnLeft
		m.conn = nil
		wasHost := userID == p.hostUserID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.store.UpdateMemberConnectionStatus(ctx, p.ID, userID, dbx.ConnLeft, nil); err != nil {
				p.logger.Error("persist leave failed", "party_id", p.ID, "user_id", userID, "error", err)
			}
		}()
		p.emit(Event{Type: EventLeft, UserID: userID, ItemID: p.currentEmbyItemID(), PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
		if wasHost {
			p.hostDisconnectedAt = nil
			newHost, ok := syncalg.SelectNewHost(p.connectedMembersForSelection(), userID)
			if ok {
				p.hostUserID = newHost
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := p.store.UpdatePartyHost(ctx, p.ID, newHost); err != nil {
						p.logger.Error("persist host transfer failed", "party_id", p.ID, "error", err)
					}
				}()
				p.broadcastSnapshot()
			}
		}
	})
}

// HandleControl applies a host-issued play/pause/seek command. Returns
// ErrNotHost if userID is not the current host — enforced here, not just
// hidden in the UI.
func (p *Party) HandleControl(userID string, msgType wsproto.MessageType, positionTicks int64) error {
	var outErr error
	p.do(func() {
		if p.ended {
			outErr = ErrPartyEnded
			return
		}
		if userID != p.hostUserID {
			outErr = ErrNotHost
			p.sendError(p.members[userID], wsproto.ErrCodeNotHost, "only the host may control playback")
			return
		}
		uid := userID
		switch msgType {
		case wsproto.MsgPlay:
			p.state = State{PositionTicks: positionTicks, IsPlaying: true, SequenceNumber: p.state.SequenceNumber + 1, ServerTimestamp: time.Now(), UpdatedByUserID: &uid, UpdatedByClientType: dbx.ClientTypeHost}
		case wsproto.MsgPause:
			p.state = State{PositionTicks: positionTicks, IsPlaying: false, SequenceNumber: p.state.SequenceNumber + 1, ServerTimestamp: time.Now(), UpdatedByUserID: &uid, UpdatedByClientType: dbx.ClientTypeHost}
		case wsproto.MsgSeek:
			p.state = State{PositionTicks: positionTicks, IsPlaying: p.state.IsPlaying, SequenceNumber: p.state.SequenceNumber + 1, ServerTimestamp: time.Now(), UpdatedByUserID: &uid, UpdatedByClientType: dbx.ClientTypeHost}
		default:
			outErr = fmt.Errorf("party: unsupported control message %q", msgType)
			return
		}
		p.broadcastControl(msgType)
		p.persistStateAsync()
		p.emit(Event{Type: EventStateChanged, UserID: userID, ItemID: p.currentEmbyItemID(), PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
	})
	return outErr
}

// HandleHostTransfer processes an explicit, host-initiated transfer to
// another connected member.
func (p *Party) HandleHostTransfer(requestingUserID, newHostUserID string) error {
	var outErr error
	p.do(func() {
		if p.ended {
			outErr = ErrPartyEnded
			return
		}
		if requestingUserID != p.hostUserID {
			outErr = ErrNotHost
			p.sendError(p.members[requestingUserID], wsproto.ErrCodeNotHost, "only the host may transfer host status")
			return
		}
		target, ok := p.members[newHostUserID]
		if !ok || target.ConnectionStatus != dbx.ConnConnected {
			outErr = ErrUnknownTarget
			p.sendError(p.members[requestingUserID], wsproto.ErrCodeBadRequest, "target user is not a connected member")
			return
		}
		p.hostUserID = newHostUserID
		p.hostDisconnectedAt = nil
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.store.UpdatePartyHost(ctx, p.ID, newHostUserID); err != nil {
				p.logger.Error("persist host transfer failed", "party_id", p.ID, "error", err)
			}
		}()
		p.broadcastSnapshot()
	})
	return outErr
}

// SelectCurrentItem is a host-only, explicit "play this playlist item now"
// command — the same transitionCurrent path checkEndOfMedia's auto-advance
// uses, just host-attributed and always starting in a playing state (there's
// no separate "load paused" step: selecting an item is starting it).
func (p *Party) SelectCurrentItem(userID string, playlistItemID int64) error {
	var outErr error
	p.do(func() {
		if p.ended {
			outErr = ErrPartyEnded
			return
		}
		if userID != p.hostUserID {
			outErr = ErrNotHost
			p.sendError(p.members[userID], wsproto.ErrCodeNotHost, "only the host may select what plays")
			return
		}
		var target *dbx.PlaylistItem
		for i := range p.playlist {
			if p.playlist[i].ID == playlistItemID {
				target = &p.playlist[i]
				break
			}
		}
		if target == nil {
			outErr = ErrPlaylistItemNotFound
			p.sendError(p.members[userID], wsproto.ErrCodeBadRequest, "playlist item not found")
			return
		}
		uid := userID
		p.transitionCurrent(&currentItem{PlaylistItemID: target.ID, EmbyItemID: target.ItemID, DurationTicks: target.DurationTicks}, true, &uid, dbx.ClientTypeHost)
	})
	return outErr
}

// checkHostAuthorized is the shared do()-dispatched authorization gate
// every playlist mutation starts with: not ended, and userID is the current
// host. Kept as a fast, IO-free check so the actual SQLite write (in the
// caller, between two do() dispatches — see AddPlaylistItem for why) never
// runs inside the actor goroutine, per this codebase's rule that party
// state mutation never blocks on synchronous IO (internal/dbx: "the sync
// loop must never block on a SQLite write").
func (p *Party) checkHostAuthorized(userID string) error {
	var outErr error
	p.do(func() {
		if p.ended {
			outErr = ErrPartyEnded
			return
		}
		if userID != p.hostUserID {
			outErr = ErrNotHost
			p.sendError(p.members[userID], wsproto.ErrCodeNotHost, "only the host may edit the playlist")
		}
	})
	return outErr
}

// AddPlaylistItem appends itemID (with its Emby-reported durationTicks,
// fetched by the caller before invoking this — the actor itself has no
// Emby access, by design) to the end of the party's queue. Host-only, like
// every other playlist mutation.
//
// The SQLite write happens between two do() dispatches, not inside one:
// authorize first (fast, IO-free), then write, then apply the result to the
// actor's in-memory cache and broadcast (also fast) — never blocking the
// single actor goroutine, and everyone else's sync traffic for this party,
// on a disk write. This does leave a small window where the party could end
// or the host could change between the authorization check and the write
// landing; worst case a row is added moments after it stops mattering,
// which is harmless and consistent with how loosely this project already
// treats sub-second imprecision around SQLite persistence elsewhere (e.g.
// CurrentPositionTicks).
func (p *Party) AddPlaylistItem(ctx context.Context, userID, itemID string, durationTicks int64) (dbx.PlaylistItem, error) {
	if err := p.checkHostAuthorized(userID); err != nil {
		return dbx.PlaylistItem{}, err
	}
	item, err := p.store.AddPlaylistItem(ctx, p.ID, itemID, durationTicks, userID, time.Now())
	if err != nil {
		return dbx.PlaylistItem{}, err
	}
	p.do(func() {
		if p.ended {
			return
		}
		p.playlist = append(p.playlist, *item)
		p.broadcastPlaylistUpdated()
	})
	return *item, nil
}

// RemovePlaylistItem deletes a queued item. Host-only. Removing the
// currently-playing item is rejected outright (ErrCannotRemoveCurrentItem)
// rather than implicitly advancing or going idle — the host must choose
// what happens next (select another item, or let it play out) explicitly.
// See AddPlaylistItem for why the SQLite write sits between two do()
// dispatches instead of inside one.
func (p *Party) RemovePlaylistItem(ctx context.Context, userID string, playlistItemID int64) error {
	if err := p.checkHostAuthorized(userID); err != nil {
		return err
	}
	var checkErr error
	p.do(func() {
		if p.current != nil && p.current.PlaylistItemID == playlistItemID {
			checkErr = ErrCannotRemoveCurrentItem
			p.sendError(p.members[userID], wsproto.ErrCodeBadRequest, "cannot remove the currently-playing item")
			return
		}
		found := false
		for _, it := range p.playlist {
			if it.ID == playlistItemID {
				found = true
				break
			}
		}
		if !found {
			checkErr = ErrPlaylistItemNotFound
		}
	})
	if checkErr != nil {
		return checkErr
	}

	if err := p.store.RemovePlaylistItem(ctx, playlistItemID); err != nil {
		return err
	}
	p.do(func() {
		for i, it := range p.playlist {
			if it.ID == playlistItemID {
				p.playlist = append(p.playlist[:i], p.playlist[i+1:]...)
				break
			}
		}
		p.broadcastPlaylistUpdated()
	})
	return nil
}

// ReorderPlaylist rewrites queue order. orderedIDs must contain exactly the
// party's current playlist item ids (any set mismatch — stale client view,
// concurrent add/remove — is rejected rather than guessed at). Host-only.
// See AddPlaylistItem for why the SQLite write sits between two do()
// dispatches instead of inside one.
func (p *Party) ReorderPlaylist(ctx context.Context, userID string, orderedIDs []int64) error {
	if err := p.checkHostAuthorized(userID); err != nil {
		return err
	}
	var reordered []dbx.PlaylistItem
	var checkErr error
	p.do(func() {
		if len(orderedIDs) != len(p.playlist) {
			checkErr = ErrInvalidReorder
			return
		}
		byID := make(map[int64]dbx.PlaylistItem, len(p.playlist))
		for _, it := range p.playlist {
			byID[it.ID] = it
		}
		reordered = make([]dbx.PlaylistItem, len(orderedIDs))
		for i, id := range orderedIDs {
			it, ok := byID[id]
			if !ok {
				checkErr = ErrInvalidReorder
				return
			}
			it.Position = i
			reordered[i] = it
		}
	})
	if checkErr != nil {
		return checkErr
	}

	if err := p.store.ReorderPlaylistItems(ctx, p.ID, orderedIDs); err != nil {
		return err
	}
	p.do(func() {
		p.playlist = reordered
		p.broadcastPlaylistUpdated()
	})
	return nil
}

// Snapshot returns the current authoritative snapshot (e.g. for HTTP
// polling / diagnostics / the Emby-reporting side channel).
func (p *Party) Snapshot() wsproto.SnapshotPayload {
	var snap wsproto.SnapshotPayload
	p.do(func() { snap = p.buildSnapshot() })
	return snap
}

// CurrentPositionTicks returns where this party actually is *right now* —
// unlike the raw State.PositionTicks captured in Snapshot, which is a
// point-in-time value as of the last state change and, while playing,
// drifts further from reality the longer it's been since then. Used when
// starting a new Emby transcode session (e.g. for someone joining a party
// already in progress) so Emby begins encoding at roughly the right offset
// instead of from the beginning — see ARCHITECTURE.md §5.4. A few seconds
// of imprecision here is fine: the client hard-seeks to the precise
// position once its own manifest loads, same as always.
func (p *Party) CurrentPositionTicks() int64 {
	var ticks int64
	p.do(func() { ticks = syncalg.ExpectedPosition(p.state.toAlg(), time.Now(), 0) })
	return ticks
}

// HostUserID returns the current host (helper for authorization checks
// that happen outside the actor, e.g. at the HTTP layer before a WS
// upgrade).
func (p *Party) HostUserID() string {
	var host string
	p.do(func() { host = p.hostUserID })
	return host
}

// End transitions the party to ended: broadcasts a final snapshot, closes
// every connection, and persists the terminal status immediately
// (synchronously, unlike routine state persistence) since ending is
// infrequent and its durability matters more than avoiding a blocking
// write here.
func (p *Party) End(ctx context.Context) error {
	var outErr error
	var finalPosition int64
	var finalIsPlaying bool
	var finalItemID string
	var alreadyEnded bool
	p.do(func() {
		if p.ended {
			alreadyEnded = true
			return
		}
		p.ended = true
		// Captured here, synchronously inside the actor, and carried on the
		// emitted Event below — not re-derived later by a consumer looking
		// the party back up in the hub, since Hub.EndParty removes it
		// immediately after this call returns.
		finalPosition = syncalg.ExpectedPosition(p.state.toAlg(), time.Now(), 0)
		finalIsPlaying = p.state.IsPlaying
		finalItemID = p.currentEmbyItemID()
		p.broadcastSnapshot()
		for _, m := range p.members {
			if m.conn != nil {
				m.conn.Close(1000, "party ended")
				m.conn = nil
			}
		}
	})
	if alreadyEnded {
		return nil
	}
	if err := p.store.UpdatePartyStatus(ctx, p.ID, dbx.PartyStatusEnded); err != nil {
		outErr = err
	}
	p.emit(Event{Type: EventEnded, ItemID: finalItemID, PositionTicks: finalPosition, IsPlaying: finalIsPlaying})
	return outErr
}

// FlushSync persists current state synchronously. Routine persistence is
// fire-and-forget (persistStateAsync) so the sync loop never blocks on
// SQLite, but graceful shutdown must guarantee the final write completes
// before the process exits, so it uses this instead.
func (p *Party) FlushSync(ctx context.Context) error {
	var st State
	p.do(func() { st = p.state })
	row := dbx.PlaybackState{
		PartyID: p.ID, PositionTicks: st.PositionTicks, IsPlaying: st.IsPlaying,
		SequenceNumber: st.SequenceNumber, ServerTimestamp: st.ServerTimestamp,
		UpdatedByUserID: st.UpdatedByUserID, UpdatedByClientType: st.UpdatedByClientType,
	}
	return p.store.UpsertPlaybackState(ctx, row)
}

// stop terminates the actor goroutine without touching the DB (used by the
// hub during graceful shutdown, after state has already been flushed).
func (p *Party) stop() {
	close(p.stopCh)
	<-p.stopped
}
