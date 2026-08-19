// Package wsproto defines the WebSocket wire protocol between a browser
// client and a party's server-side actor. See ARCHITECTURE.md §3.
package wsproto

import "encoding/json"

// ProtocolVersion is carried on every message so future incompatible
// changes can be detected by either side.
const ProtocolVersion = 1

type MessageType string

const (
	// Control events — host-only, authoritative state changes.
	MsgPlay         MessageType = "play"
	MsgPause        MessageType = "pause"
	MsgSeek         MessageType = "seek"
	MsgHostTransfer MessageType = "host_transfer"

	// Lifecycle events.
	MsgJoin  MessageType = "join"
	MsgLeave MessageType = "leave"

	// Transport/control frames.
	MsgPing      MessageType = "ping"
	MsgPong      MessageType = "pong"
	MsgClockSync MessageType = "clock_sync"
	MsgSnapshot  MessageType = "snapshot"

	// Server -> client: the party's playlist (queue) changed (add/remove/
	// reorder). Does NOT cover a change of *current* item — that's an
	// authoritative state change and goes through the existing snapshot
	// broadcast (ItemID/DurationTicks live on SnapshotPayload already).
	// Carries only the raw ordered rows: the party actor has no Emby access
	// (by design), so it cannot resolve titles/posters itself — clients
	// treat this as an invalidation signal and re-fetch
	// GET /api/parties/{id}/playlist to resolve metadata under their own
	// token (same per-viewer-resolution pattern used elsewhere).
	MsgPlaylistUpdated MessageType = "playlist_updated"

	// Server -> client informational.
	MsgError MessageType = "error"
)

// Envelope is the outer shape of every frame in both directions.
type Envelope struct {
	ProtocolVersion int             `json:"protocol_version"`
	Type            MessageType     `json:"type"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// ControlPayload carries the client's view of position for play/pause/seek.
// The server treats this as a hint for where to timestamp the new
// authoritative state from — it does not trust it blindly for anything
// beyond that (e.g. it's never forwarded to Emby as a user's reported
// position; see the emby reporting package for why).
type ControlPayload struct {
	PositionTicks int64 `json:"position_ticks"`
}

type HostTransferPayload struct {
	NewHostUserID string `json:"new_host_user_id"`
}

// AuthoritativeState is embedded in every broadcast state change and in
// snapshots. Field names match the spec's wire-format requirements exactly.
type AuthoritativeState struct {
	PositionTicks       int64   `json:"position_ticks"`
	IsPlaying           bool    `json:"is_playing"`
	ServerTimestamp     string  `json:"server_timestamp"` // RFC3339Nano; see ARCHITECTURE.md §1.5
	SequenceNumber      int64   `json:"sequence_number"`
	UpdatedByUserID     *string `json:"updated_by_user_id,omitempty"`
	UpdatedByClientType string  `json:"updated_by_client_type"`
}

// MemberInfo describes one party participant for display purposes.
type MemberInfo struct {
	UserID           string `json:"user_id"`
	DisplayName      string `json:"display_name"`
	ConnectionStatus string `json:"connection_status"`
	IsHost           bool   `json:"is_host"`
}

// SnapshotPayload is the complete authoritative state sent on join,
// reconnect, and periodically thereafter. A client never needs to replay
// historical events — it always has everything it needs from the latest
// snapshot plus whatever authoritative broadcasts arrive after it.
type SnapshotPayload struct {
	AuthoritativeState
	PartyID       string       `json:"party_id"`
	ItemID        string       `json:"item_id"`
	DurationTicks int64        `json:"duration_ticks"`
	HostUserID    string       `json:"host_user_id"`
	Members       []MemberInfo `json:"members"`
}

// ClockSyncPing is sent client -> server to begin the handshake. Beyond t0,
// it optionally carries the client's own most recent sync diagnostics —
// its current player position, and the RTT/offset/correction action it
// computed from the *previous* handshake — purely so the server can emit
// the debug-level sync diagnostics logging the spec requires (party ID,
// user ID, sequence, authoritative-vs-client position, drift, RTT, clock
// offset, action taken). None of these fields affect server behavior; the
// server never trusts client-reported position for anything authoritative
// (see the emby reporting package) — this is diagnostics-only, piggybacked
// on the clock-sync frame rather than inventing a new message type outside
// the spec's three message categories.
type ClockSyncPing struct {
	T0                   int64  `json:"t0"`
	PositionTicks        *int64 `json:"position_ticks,omitempty"`
	LastRTTMs            *int64 `json:"last_rtt_ms,omitempty"`
	LastClockOffsetMs    *int64 `json:"last_clock_offset_ms,omitempty"`
	LastCorrectionAction string `json:"last_correction_action,omitempty"` // "none" | "nudge_rate" | "hard_seek"
}

// ClockSyncPong is the server's reply.
type ClockSyncPong struct {
	T0 int64 `json:"t0"` // echoed back unchanged
	T1 int64 `json:"t1"` // server receive time, ms since Unix epoch
	T2 int64 `json:"t2"` // server send time, ms since Unix epoch
}

// PlaylistItemRef is one entry in a MsgPlaylistUpdated broadcast — just
// enough to identify and order items; no Emby-resolved metadata (see
// MsgPlaylistUpdated's doc comment).
type PlaylistItemRef struct {
	ID       int64  `json:"id"`
	ItemID   string `json:"item_id"`
	Position int    `json:"position"`
}

// PlaylistUpdatedPayload is the complete current queue, broadcast whenever
// it changes (add/remove/reorder).
type PlaylistUpdatedPayload struct {
	Items []PlaylistItemRef `json:"items"`
}

type JoinPayload struct {
	UserID string `json:"user_id"`
}

type LeavePayload struct {
	UserID string `json:"user_id"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes returned in ErrorPayload.Code.
const (
	ErrCodeNotHost      = "not_host"
	ErrCodeInvalidState = "invalid_state"
	ErrCodePartyEnded   = "party_ended"
	ErrCodeBadRequest   = "bad_request"
	ErrCodeUnauthorized = "unauthorized"
)
