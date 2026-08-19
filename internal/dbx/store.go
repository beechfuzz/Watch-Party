package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by Get-style methods when no row matches.
var ErrNotFound = errors.New("dbx: not found")

// Store is the data access layer over the five app tables. It holds no
// business logic beyond basic upsert/get shapes; sync-protocol semantics
// live in the party package, which uses Store purely for durability.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// --- users ---

func (s *Store) UpsertUser(ctx context.Context, id, displayName, encryptedAccessToken string, lastSeenAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, encrypted_access_token, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			encrypted_access_token = excluded.encrypted_access_token,
			last_seen_at = excluded.last_seen_at
	`, id, displayName, encryptedAccessToken, timeStr(lastSeenAt))
	if err != nil {
		return fmt.Errorf("dbx: upsert user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, encrypted_access_token, last_seen_at FROM users WHERE id = ?
	`, id)
	var u User
	var lastSeen string
	if err := row.Scan(&u.ID, &u.DisplayName, &u.EncryptedAccessToken, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dbx: get user: %w", err)
	}
	t, err := parseTime(lastSeen)
	if err != nil {
		return nil, fmt.Errorf("dbx: get user: %w", err)
	}
	u.LastSeenAt = t
	return &u, nil
}

// --- sessions ---

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, created_at, last_seen_at, expires_at, csrf_token)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sess.ID, sess.UserID, timeStr(sess.CreatedAt), timeStr(sess.LastSeenAt), timeStr(sess.ExpiresAt), sess.CSRFToken)
	if err != nil {
		return fmt.Errorf("dbx: create session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, created_at, last_seen_at, expires_at, csrf_token FROM sessions WHERE id = ?
	`, id)
	var sess Session
	var created, lastSeen, expires string
	if err := row.Scan(&sess.ID, &sess.UserID, &created, &lastSeen, &expires, &sess.CSRFToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dbx: get session: %w", err)
	}
	var err error
	if sess.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("dbx: get session: %w", err)
	}
	if sess.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return nil, fmt.Errorf("dbx: get session: %w", err)
	}
	if sess.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, fmt.Errorf("dbx: get session: %w", err)
	}
	return &sess, nil
}

func (s *Store) TouchSession(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, timeStr(now), id)
	if err != nil {
		return fmt.Errorf("dbx: touch session: %w", err)
	}
	return nil
}

// DeleteSessionsForUser is used when a user's Emby AccessToken is rejected
// by Emby (revoked/expired password change etc.) — this forces re-auth
// rather than silently retrying a dead token indefinitely.
func (s *Store) DeleteSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("dbx: delete sessions for user: %w", err)
	}
	return nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dbx: delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions past their absolute expiry. Callers
// are expected to run this periodically (it is not triggered per-request).
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, timeStr(now))
	if err != nil {
		return fmt.Errorf("dbx: delete expired sessions: %w", err)
	}
	return nil
}

// --- parties ---

func (s *Store) CreateParty(ctx context.Context, p Party) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO parties (id, host_user_id, name, current_playlist_item_id, created_at, status,
			auto_advance, show_next_dialog, autoplay_enabled, autoplay_delay_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, p.HostUserID, p.Name, p.CurrentPlaylistItemID, timeStr(p.CreatedAt), string(p.Status),
		p.AutoAdvance, p.ShowNextDialog, p.AutoplayEnabled, p.AutoplayDelaySeconds)
	if err != nil {
		return fmt.Errorf("dbx: create party: %w", err)
	}
	return nil
}

func (s *Store) GetParty(ctx context.Context, id string) (*Party, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, host_user_id, name, current_playlist_item_id, created_at, status,
			auto_advance, show_next_dialog, autoplay_enabled, autoplay_delay_seconds
		FROM parties WHERE id = ?
	`, id)
	return scanParty(row)
}

func scanParty(row *sql.Row) (*Party, error) {
	var p Party
	var created, status string
	var currentItemID sql.NullInt64
	if err := row.Scan(&p.ID, &p.HostUserID, &p.Name, &currentItemID, &created, &status,
		&p.AutoAdvance, &p.ShowNextDialog, &p.AutoplayEnabled, &p.AutoplayDelaySeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dbx: get party: %w", err)
	}
	t, err := parseTime(created)
	if err != nil {
		return nil, fmt.Errorf("dbx: get party: %w", err)
	}
	p.CreatedAt = t
	p.Status = PartyStatus(status)
	if currentItemID.Valid {
		p.CurrentPlaylistItemID = &currentItemID.Int64
	}
	return &p, nil
}

func (s *Store) UpdatePartyStatus(ctx context.Context, id string, status PartyStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE parties SET status = ? WHERE id = ?`, string(status), id)
	if err != nil {
		return fmt.Errorf("dbx: update party status: %w", err)
	}
	return nil
}

func (s *Store) UpdatePartyHost(ctx context.Context, id, newHostUserID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE parties SET host_user_id = ? WHERE id = ?`, newHostUserID, id)
	if err != nil {
		return fmt.Errorf("dbx: update party host: %w", err)
	}
	return nil
}

// UpdatePartyCurrentItem persists which playlist item (if any) is currently
// loaded — set on playlist advance (end-of-media) and on a host explicitly
// selecting an item to play. A nil itemID means idle: nothing loaded.
func (s *Store) UpdatePartyCurrentItem(ctx context.Context, id string, playlistItemID *int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE parties SET current_playlist_item_id = ? WHERE id = ?`, playlistItemID, id)
	if err != nil {
		return fmt.Errorf("dbx: update party current item: %w", err)
	}
	return nil
}

// UpdatePartySettings persists a host's edits from the Party Settings form —
// name plus the four end-of-media settings, updated together since they
// share one form (see ARCHITECTURE.md's Party Settings section).
func (s *Store) UpdatePartySettings(ctx context.Context, id, name string, autoAdvance, showNextDialog, autoplayEnabled bool, autoplayDelaySeconds int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE parties SET name = ?, auto_advance = ?, show_next_dialog = ?, autoplay_enabled = ?, autoplay_delay_seconds = ?
		WHERE id = ?
	`, name, autoAdvance, showNextDialog, autoplayEnabled, autoplayDelaySeconds, id)
	if err != nil {
		return fmt.Errorf("dbx: update party settings: %w", err)
	}
	return nil
}

// ListActiveParties returns every party with status='active', for startup
// recovery: on boot the server reconstructs in-memory state for each.
func (s *Store) ListActiveParties(ctx context.Context) ([]Party, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, host_user_id, name, current_playlist_item_id, created_at, status,
			auto_advance, show_next_dialog, autoplay_enabled, autoplay_delay_seconds
		FROM parties WHERE status = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("dbx: list active parties: %w", err)
	}
	defer rows.Close()
	var out []Party
	for rows.Next() {
		var p Party
		var created, status string
		var currentItemID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.HostUserID, &p.Name, &currentItemID, &created, &status,
			&p.AutoAdvance, &p.ShowNextDialog, &p.AutoplayEnabled, &p.AutoplayDelaySeconds); err != nil {
			return nil, fmt.Errorf("dbx: list active parties: %w", err)
		}
		t, err := parseTime(created)
		if err != nil {
			return nil, fmt.Errorf("dbx: list active parties: %w", err)
		}
		p.CreatedAt = t
		p.Status = PartyStatus(status)
		if currentItemID.Valid {
			p.CurrentPlaylistItemID = &currentItemID.Int64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- playlist_items ---

// AddPlaylistItem inserts a new playlist entry at the end of the party's
// queue (position = current max + 1, or 0 if empty) and returns the
// complete row, including the assigned id and position.
func (s *Store) AddPlaylistItem(ctx context.Context, partyID, itemID string, durationTicks int64, addedByUserID string, addedAt time.Time) (*PlaylistItem, error) {
	var nextPos int
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM playlist_items WHERE party_id = ?`, partyID)
	if err := row.Scan(&nextPos); err != nil {
		return nil, fmt.Errorf("dbx: add playlist item: compute position: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO playlist_items (party_id, item_id, duration_ticks, position, added_by_user_id, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, partyID, itemID, durationTicks, nextPos, addedByUserID, timeStr(addedAt))
	if err != nil {
		return nil, fmt.Errorf("dbx: add playlist item: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("dbx: add playlist item: %w", err)
	}
	return &PlaylistItem{
		ID: id, PartyID: partyID, ItemID: itemID, DurationTicks: durationTicks,
		Position: nextPos, AddedByUserID: addedByUserID, AddedAt: addedAt,
	}, nil
}

// AddPlaylistItems inserts multiple playlist entries at the end of the
// party's queue in one transaction — the batch counterpart to
// AddPlaylistItem, used by the "Add N Selected Item(s)" flow so a large
// selection is one atomic write (and, at the party actor level, one
// playlist_updated broadcast) instead of N. Positions are assigned
// sequentially starting after the current max, preserving items' order.
func (s *Store) AddPlaylistItems(ctx context.Context, partyID string, items []NewPlaylistItem, addedByUserID string, addedAt time.Time) ([]PlaylistItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dbx: add playlist items: %w", err)
	}
	defer tx.Rollback()

	var nextPos int
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM playlist_items WHERE party_id = ?`, partyID)
	if err := row.Scan(&nextPos); err != nil {
		return nil, fmt.Errorf("dbx: add playlist items: compute position: %w", err)
	}

	out := make([]PlaylistItem, 0, len(items))
	for i, item := range items {
		pos := nextPos + i
		res, err := tx.ExecContext(ctx, `
			INSERT INTO playlist_items (party_id, item_id, duration_ticks, position, added_by_user_id, added_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, partyID, item.ItemID, item.DurationTicks, pos, addedByUserID, timeStr(addedAt))
		if err != nil {
			return nil, fmt.Errorf("dbx: add playlist items: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("dbx: add playlist items: %w", err)
		}
		out = append(out, PlaylistItem{
			ID: id, PartyID: partyID, ItemID: item.ItemID, DurationTicks: item.DurationTicks,
			Position: pos, AddedByUserID: addedByUserID, AddedAt: addedAt,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dbx: add playlist items: %w", err)
	}
	return out, nil
}

// ListPlaylistItems returns a party's queue in order.
func (s *Store) ListPlaylistItems(ctx context.Context, partyID string) ([]PlaylistItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, party_id, item_id, duration_ticks, position, added_by_user_id, added_at
		FROM playlist_items WHERE party_id = ? ORDER BY position ASC
	`, partyID)
	if err != nil {
		return nil, fmt.Errorf("dbx: list playlist items: %w", err)
	}
	defer rows.Close()
	var out []PlaylistItem
	for rows.Next() {
		item, err := scanPlaylistItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanPlaylistItem(rows *sql.Rows) (PlaylistItem, error) {
	var item PlaylistItem
	var addedAt string
	if err := rows.Scan(&item.ID, &item.PartyID, &item.ItemID, &item.DurationTicks, &item.Position, &item.AddedByUserID, &addedAt); err != nil {
		return item, fmt.Errorf("dbx: scan playlist item: %w", err)
	}
	t, err := parseTime(addedAt)
	if err != nil {
		return item, fmt.Errorf("dbx: scan playlist item: %w", err)
	}
	item.AddedAt = t
	return item, nil
}

func (s *Store) GetPlaylistItem(ctx context.Context, id int64) (*PlaylistItem, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, party_id, item_id, duration_ticks, position, added_by_user_id, added_at
		FROM playlist_items WHERE id = ?
	`, id)
	var item PlaylistItem
	var addedAt string
	if err := row.Scan(&item.ID, &item.PartyID, &item.ItemID, &item.DurationTicks, &item.Position, &item.AddedByUserID, &addedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dbx: get playlist item: %w", err)
	}
	t, err := parseTime(addedAt)
	if err != nil {
		return nil, fmt.Errorf("dbx: get playlist item: %w", err)
	}
	item.AddedAt = t
	return &item, nil
}

func (s *Store) RemovePlaylistItem(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM playlist_items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dbx: remove playlist item: %w", err)
	}
	return nil
}

// ReorderPlaylistItems rewrites position values to match orderedIDs (must be
// exactly the party's current set of playlist item ids, in the new order).
// Positions are moved through a temporary negative range first, since
// idx_playlist_items_party_position is a non-deferrable unique index in
// SQLite — writing final positions directly, one row at a time, would
// collide with whatever row currently holds the destination position (e.g.
// swapping two adjacent items).
func (s *Store) ReorderPlaylistItems(ctx context.Context, partyID string, orderedIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dbx: reorder playlist items: %w", err)
	}
	defer tx.Rollback()
	for i, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE playlist_items SET position = ? WHERE id = ? AND party_id = ?`, -(i + 1), id, partyID); err != nil {
			return fmt.Errorf("dbx: reorder playlist items: %w", err)
		}
	}
	for i, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE playlist_items SET position = ? WHERE id = ? AND party_id = ?`, i, id, partyID); err != nil {
			return fmt.Errorf("dbx: reorder playlist items: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dbx: reorder playlist items: %w", err)
	}
	return nil
}

// --- party_members ---

func (s *Store) UpsertMember(ctx context.Context, m PartyMember) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO party_members (party_id, user_id, joined_at, last_connected_at, connection_status, last_reported_position_ticks, last_reported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(party_id, user_id) DO UPDATE SET
			last_connected_at = excluded.last_connected_at,
			connection_status = excluded.connection_status,
			last_reported_position_ticks = excluded.last_reported_position_ticks,
			last_reported_at = excluded.last_reported_at
	`, m.PartyID, m.UserID, timeStr(m.JoinedAt), nullableTimeStr(m.LastConnectedAt),
		string(m.ConnectionStatus), m.LastReportedPositionTicks, nullableTimeStr(m.LastReportedAt))
	if err != nil {
		return fmt.Errorf("dbx: upsert member: %w", err)
	}
	return nil
}

func (s *Store) UpdateMemberConnectionStatus(ctx context.Context, partyID, userID string, status ConnectionStatus, lastConnectedAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE party_members SET connection_status = ?, last_connected_at = COALESCE(?, last_connected_at)
		WHERE party_id = ? AND user_id = ?
	`, string(status), nullableTimeStr(lastConnectedAt), partyID, userID)
	if err != nil {
		return fmt.Errorf("dbx: update member connection status: %w", err)
	}
	return nil
}

func (s *Store) UpdateMemberLastReported(ctx context.Context, partyID, userID string, positionTicks int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE party_members SET last_reported_position_ticks = ?, last_reported_at = ?
		WHERE party_id = ? AND user_id = ?
	`, positionTicks, timeStr(at), partyID, userID)
	if err != nil {
		return fmt.Errorf("dbx: update member last reported: %w", err)
	}
	return nil
}

func (s *Store) GetMembers(ctx context.Context, partyID string) ([]PartyMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT party_id, user_id, joined_at, last_connected_at, connection_status, last_reported_position_ticks, last_reported_at
		FROM party_members WHERE party_id = ? ORDER BY joined_at ASC
	`, partyID)
	if err != nil {
		return nil, fmt.Errorf("dbx: get members: %w", err)
	}
	defer rows.Close()
	var out []PartyMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMember(rows *sql.Rows) (PartyMember, error) {
	var m PartyMember
	var joined string
	var lastConnected, lastReportedAt sql.NullString
	var status string
	if err := rows.Scan(&m.PartyID, &m.UserID, &joined, &lastConnected, &status, &m.LastReportedPositionTicks, &lastReportedAt); err != nil {
		return m, fmt.Errorf("dbx: scan member: %w", err)
	}
	t, err := parseTime(joined)
	if err != nil {
		return m, fmt.Errorf("dbx: scan member: %w", err)
	}
	m.JoinedAt = t
	m.ConnectionStatus = ConnectionStatus(status)
	if lastConnected.Valid {
		lt, err := parseTime(lastConnected.String)
		if err != nil {
			return m, fmt.Errorf("dbx: scan member: %w", err)
		}
		m.LastConnectedAt = &lt
	}
	if lastReportedAt.Valid {
		lt, err := parseTime(lastReportedAt.String)
		if err != nil {
			return m, fmt.Errorf("dbx: scan member: %w", err)
		}
		m.LastReportedAt = &lt
	}
	return m, nil
}

// --- playback_state ---

func (s *Store) UpsertPlaybackState(ctx context.Context, st PlaybackState) error {
	var updatedBy any
	if st.UpdatedByUserID != nil {
		updatedBy = *st.UpdatedByUserID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO playback_state (party_id, position_ticks, is_playing, sequence_number, server_timestamp, updated_by_user_id, updated_by_client_type)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(party_id) DO UPDATE SET
			position_ticks = excluded.position_ticks,
			is_playing = excluded.is_playing,
			sequence_number = excluded.sequence_number,
			server_timestamp = excluded.server_timestamp,
			updated_by_user_id = excluded.updated_by_user_id,
			updated_by_client_type = excluded.updated_by_client_type
		WHERE excluded.sequence_number >= playback_state.sequence_number
	`, st.PartyID, st.PositionTicks, st.IsPlaying, st.SequenceNumber, timeStr(st.ServerTimestamp), updatedBy, string(st.UpdatedByClientType))
	if err != nil {
		return fmt.Errorf("dbx: upsert playback state: %w", err)
	}
	return nil
}

func (s *Store) GetPlaybackState(ctx context.Context, partyID string) (*PlaybackState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT party_id, position_ticks, is_playing, sequence_number, server_timestamp, updated_by_user_id, updated_by_client_type
		FROM playback_state WHERE party_id = ?
	`, partyID)
	var st PlaybackState
	var ts string
	var updatedBy sql.NullString
	var clientType string
	if err := row.Scan(&st.PartyID, &st.PositionTicks, &st.IsPlaying, &st.SequenceNumber, &ts, &updatedBy, &clientType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dbx: get playback state: %w", err)
	}
	t, err := parseTime(ts)
	if err != nil {
		return nil, fmt.Errorf("dbx: get playback state: %w", err)
	}
	st.ServerTimestamp = t
	st.UpdatedByClientType = ClientType(clientType)
	if updatedBy.Valid {
		v := updatedBy.String
		st.UpdatedByUserID = &v
	}
	return &st, nil
}
