CREATE TABLE chat_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id TEXT NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id),
    body TEXT NOT NULL,
    sent_at TEXT NOT NULL              -- RFC3339Nano, server-assigned
);
CREATE INDEX idx_chat_messages_party_id ON chat_messages(party_id, id);
