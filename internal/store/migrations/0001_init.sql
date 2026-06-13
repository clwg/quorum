CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT,
  role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin','bot')),
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash  BLOB PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE identity_keys (
  user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  public_key BLOB NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE channels (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
  topic      TEXT NOT NULL DEFAULT '',
  created_by TEXT REFERENCES users(id),
  created_at INTEGER NOT NULL
);

CREATE TABLE channel_members (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  joined_at  INTEGER NOT NULL,
  PRIMARY KEY (channel_id, user_id)
);
CREATE INDEX idx_channel_members_user ON channel_members(user_id);

-- Group messages only; 1:1 E2EE traffic is relayed and never lands here.
CREATE TABLE messages (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  sender_id  TEXT NOT NULL REFERENCES users(id),
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_messages_channel ON messages(channel_id, id);

CREATE TABLE bots (
  user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  owner_id   TEXT NOT NULL REFERENCES users(id),
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);
