package dbx

import "time"

type User struct {
	ID                   string
	DisplayName          string
	EncryptedAccessToken string
	LastSeenAt           time.Time
}

type Session struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	CSRFToken  string
}

type PartyStatus string

const (
	PartyStatusCreated PartyStatus = "created"
	PartyStatusActive  PartyStatus = "active"
	PartyStatusEnded   PartyStatus = "ended"
)

type Party struct {
	ID                    string
	HostUserID            string
	Name                  string
	CurrentPlaylistItemID *int64
	CreatedAt             time.Time
	Status                PartyStatus

	// Party Settings: govern end-of-media behavior. See ARCHITECTURE.md's
	// Party Settings section.
	AutoAdvance          bool // false: end-of-media always goes idle, same as an exhausted queue
	ShowNextDialog       bool // display-only; the server-side countdown/transition happens regardless
	AutoplayEnabled      bool // whether the next item starts playing, or just loads paused, once the countdown ends
	AutoplayDelaySeconds int
}

// PlaylistItem is one entry in a party's queue. DurationTicks is fetched
// from Emby (using the adding user's own token) and persisted at add-time,
// authoritative from then on — the party actor has no Emby access of its
// own (by design, see ARCHITECTURE.md §3), so it must not need to re-fetch
// this when the item becomes current.
type PlaylistItem struct {
	ID            int64
	PartyID       string
	ItemID        string
	DurationTicks int64
	Position      int
	AddedByUserID string
	AddedAt       time.Time
}

// NewPlaylistItem is one item to insert via Store.AddPlaylistItems — the
// batch counterpart to AddPlaylistItem's individual itemID/durationTicks
// args.
type NewPlaylistItem struct {
	ItemID        string
	DurationTicks int64
}

type ConnectionStatus string

const (
	ConnConnected    ConnectionStatus = "connected"
	ConnDisconnected ConnectionStatus = "disconnected"
	ConnLeft         ConnectionStatus = "left"
)

type PartyMember struct {
	PartyID                   string
	UserID                    string
	JoinedAt                  time.Time
	LastConnectedAt           *time.Time
	ConnectionStatus          ConnectionStatus
	LastReportedPositionTicks int64
	LastReportedAt            *time.Time
}

// ChatMessage is one persisted party chat message. Deliberately carries no
// display name or avatar -- sender identity is resolved live, per the
// *viewing* participant's own Emby token, the same per-viewer pattern
// GetItem/ImageURL already use for playlist metadata (see ARCHITECTURE.md's
// Party chat section). Rows are deleted in bulk the moment a party ends
// (Party.End), so nothing here needs to outlive an active party.
type ChatMessage struct {
	ID      int64
	PartyID string
	UserID  string
	Body    string
	SentAt  time.Time
}

type ClientType string

const (
	ClientTypeHost   ClientType = "host"
	ClientTypeSystem ClientType = "system"
)

type PlaybackState struct {
	PartyID             string
	PositionTicks       int64
	IsPlaying           bool
	SequenceNumber      int64
	ServerTimestamp     time.Time
	UpdatedByUserID     *string
	UpdatedByClientType ClientType
}

// timeStr / parseTime centralize the RFC3339Nano wall-clock text encoding
// used for every timestamp column. See ARCHITECTURE.md's note on the
// WebSocket protocol's server_timestamp for why RFC3339Nano (a deliberate
// non-monotonic simplification) was chosen; the same format is reused here
// purely for consistency and human-readability in the DB, not because
// storage needs monotonicity.
func timeStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func nullableTimeStr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeStr(*t)
}
