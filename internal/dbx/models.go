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
	ID            string
	HostUserID    string
	ItemID        string
	DurationTicks int64
	CreatedAt     time.Time
	Status        PartyStatus
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
