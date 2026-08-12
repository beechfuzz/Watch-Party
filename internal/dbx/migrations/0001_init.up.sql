CREATE TABLE users (
    id TEXT PRIMARY KEY,                    -- Emby user ID
    display_name TEXT NOT NULL,
    encrypted_access_token TEXT NOT NULL,
    last_seen_at TEXT NOT NULL              -- RFC3339Nano
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,                    -- opaque 256-bit hex token
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,             -- advances on activity; enforces idle timeout
    expires_at TEXT NOT NULL,               -- fixed at creation; enforces absolute max age
    csrf_token TEXT NOT NULL                -- synchronizer-token CSRF secret bound to this session
);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);

CREATE TABLE parties (
    id TEXT PRIMARY KEY,                    -- random public party ID, not a sequential row id
    host_user_id TEXT NOT NULL REFERENCES users(id),
    item_id TEXT NOT NULL,                  -- Emby ItemId
    duration_ticks INTEGER NOT NULL,        -- persisted at party creation as authoritative metadata
    created_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('created', 'active', 'ended'))
);
CREATE INDEX idx_parties_status ON parties(status);

CREATE TABLE party_members (
    party_id TEXT NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id),
    joined_at TEXT NOT NULL,
    last_connected_at TEXT,
    connection_status TEXT NOT NULL CHECK (connection_status IN ('connected', 'disconnected', 'left')),
    last_reported_position_ticks INTEGER NOT NULL DEFAULT 0,
    last_reported_at TEXT,
    PRIMARY KEY (party_id, user_id)
);
CREATE INDEX idx_party_members_party_id ON party_members(party_id);

CREATE TABLE playback_state (
    party_id TEXT PRIMARY KEY REFERENCES parties(id) ON DELETE CASCADE,
    position_ticks INTEGER NOT NULL DEFAULT 0,
    is_playing INTEGER NOT NULL DEFAULT 0,
    sequence_number INTEGER NOT NULL DEFAULT 0,
    server_timestamp TEXT NOT NULL,
    updated_by_user_id TEXT REFERENCES users(id),
    updated_by_client_type TEXT NOT NULL CHECK (updated_by_client_type IN ('host', 'system'))
);
