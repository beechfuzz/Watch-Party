CREATE TABLE playlist_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id TEXT NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,              -- Emby ItemId
    duration_ticks INTEGER NOT NULL,    -- fetched from Emby at add-time, authoritative
    position INTEGER NOT NULL,          -- 0-based order within the party's queue
    added_by_user_id TEXT NOT NULL REFERENCES users(id),
    added_at TEXT NOT NULL              -- RFC3339Nano
);
CREATE UNIQUE INDEX idx_playlist_items_party_position ON playlist_items(party_id, position);
CREATE INDEX idx_playlist_items_party_id ON playlist_items(party_id);

ALTER TABLE parties ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE parties ADD COLUMN current_playlist_item_id INTEGER REFERENCES playlist_items(id) ON DELETE SET NULL;
ALTER TABLE parties DROP COLUMN item_id;
ALTER TABLE parties DROP COLUMN duration_ticks;
