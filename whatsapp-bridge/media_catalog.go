package main

// Object identity and cached-message membership are separate: purging one
// message must not invalidate the identical bytes still used by another chat.
const mediaCacheSchema = `
CREATE TABLE IF NOT EXISTS media_cache (
    sha256 BLOB PRIMARY KEY CHECK(length(sha256) = 32),
    bytes INTEGER NOT NULL CHECK(bytes >= 0),
    media_type TEXT NOT NULL CHECK(media_type IN ('image','video','audio','document','sticker','status')),
    backend TEXT NOT NULL CHECK(backend IN ('local','s3')),
    stored_at TIMESTAMP NOT NULL,
    last_access_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS media_cache_refs (
    id TEXT NOT NULL,
    chat_jid TEXT NOT NULL,
    sha256 BLOB NOT NULL CHECK(length(sha256) = 32),
    PRIMARY KEY(id, chat_jid),
    FOREIGN KEY(sha256) REFERENCES media_cache(sha256) ON DELETE CASCADE,
    FOREIGN KEY(id, chat_jid) REFERENCES messages(id, chat_jid) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_media_cache_refs_hash ON media_cache_refs(sha256);
CREATE INDEX IF NOT EXISTS idx_media_cache_refs_chat ON media_cache_refs(chat_jid);
CREATE INDEX IF NOT EXISTS idx_media_cache_stored_at ON media_cache(stored_at);
CREATE INDEX IF NOT EXISTS idx_media_cache_last_access_at ON media_cache(last_access_at);
CREATE INDEX IF NOT EXISTS idx_messages_media_cache_cursor ON messages(timestamp,chat_jid,id);
CREATE TABLE IF NOT EXISTS media_cache_deletions (
    sha256 BLOB PRIMARY KEY CHECK(length(sha256) = 32),
    FOREIGN KEY(sha256) REFERENCES media_cache(sha256) ON DELETE CASCADE
);
`
