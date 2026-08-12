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
	ErrNotHost       = errors.New("party: user is not host")
	ErrNotMember     = errors.New("party: user is not a member of this party")
	ErrPartyEnded    = errors.New("party: party has ended")
	ErrUnknownTarget = errors.New("party: target user is not an eligible connected member")
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
)

// Event is a best-effort notification; consumers must not assume delivery
// is guaranteed (the channel is bounded and full sends are dropped) since
// nothing in the sync-critical path may block on a slow consumer.
type Event struct {
	Type          EventType
	PartyID       string
	UserID        string // empty for party-scoped events with no single subject user
	PositionTicks int64
	IsPlaying     bool
}

// Party is the actor for one party: all fields below the run() goroutine
// line are only ever read/written from inside that goroutine.
type Party struct {
	ID            string
	ItemID        string
	DurationTicks int64

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
}

func newParty(id, itemID string, durationTicks int64, hostUserID string, initial State, store *dbx.Store, tuning Tuning, logger *slog.Logger, events chan<- Event) *Party {
	p := &Party{
		ID: id, ItemID: itemID, DurationTicks: durationTicks,
		store: store, tuning: tuning, logger: logger, events: events,
		cmdCh: make(chan func()), stopCh: make(chan struct{}), stopped: make(chan struct{}),
		state: initial, hostUserID: hostUserID, members: make(map[string]*member),
	}
	go p.run()
	return p
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
	return wsproto.SnapshotPayload{
		AuthoritativeState: p.state.toWire(),
		PartyID:            p.ID,
		ItemID:             p.ItemID,
		DurationTicks:      p.DurationTicks,
		HostUserID:         p.hostUserID,
		Members:            members,
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
// everyone.
func (p *Party) checkEndOfMedia() {
	if !p.state.IsPlaying || p.DurationTicks <= 0 {
		return
	}
	expected := syncalg.ExpectedPosition(p.state.toAlg(), time.Now(), 0)
	if expected < p.DurationTicks {
		return
	}
	p.state = State{
		PositionTicks: p.DurationTicks, IsPlaying: false,
		SequenceNumber: p.state.SequenceNumber + 1, ServerTimestamp: time.Now(),
		UpdatedByUserID: nil, UpdatedByClientType: dbx.ClientTypeSystem,
	}
	p.broadcastControl(wsproto.MsgPause)
	p.persistStateAsync()
	p.emit(Event{Type: EventStateChanged, PositionTicks: p.state.PositionTicks, IsPlaying: false})
	p.logger.Info("end of media reached", "party_id", p.ID)
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
		p.emit(Event{Type: EventJoined, UserID: userID, PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
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
		p.emit(Event{Type: EventDisconnected, UserID: userID})
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
		p.emit(Event{Type: EventLeft, UserID: userID})
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
		p.emit(Event{Type: EventStateChanged, UserID: userID, PositionTicks: p.state.PositionTicks, IsPlaying: p.state.IsPlaying})
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

// Snapshot returns the current authoritative snapshot (e.g. for HTTP
// polling / diagnostics / the Emby-reporting side channel).
func (p *Party) Snapshot() wsproto.SnapshotPayload {
	var snap wsproto.SnapshotPayload
	p.do(func() { snap = p.buildSnapshot() })
	return snap
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
	p.do(func() {
		if p.ended {
			return
		}
		p.ended = true
		p.broadcastSnapshot()
		for _, m := range p.members {
			if m.conn != nil {
				m.conn.Close(1000, "party ended")
				m.conn = nil
			}
		}
	})
	if err := p.store.UpdatePartyStatus(ctx, p.ID, dbx.PartyStatusEnded); err != nil {
		outErr = err
	}
	p.emit(Event{Type: EventEnded})
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
