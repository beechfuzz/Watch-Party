ALTER TABLE parties DROP COLUMN autoplay_delay_seconds;
ALTER TABLE parties DROP COLUMN autoplay_enabled;
ALTER TABLE parties DROP COLUMN show_next_dialog;
ALTER TABLE parties DROP COLUMN auto_advance;

ALTER TABLE parties ADD COLUMN item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE parties ADD COLUMN duration_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE parties DROP COLUMN current_playlist_item_id;
ALTER TABLE parties DROP COLUMN name;

DROP TABLE IF EXISTS playlist_items;
