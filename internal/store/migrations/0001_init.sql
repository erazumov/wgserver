-- 0001_init: initial schema for wgserver.
-- Forward-only. Never edit after it's been applied to a real DB.
-- The schema_version table itself is managed by the migration runner
-- in db.go; do not CREATE or DROP it here.

CREATE TABLE admins (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE peers (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    name                        TEXT    NOT NULL,
    public_key                  TEXT    NOT NULL UNIQUE,
    private_key                 TEXT    NOT NULL,
    preshared_key               TEXT,
    assigned_ip                 TEXT    NOT NULL UNIQUE,
    created_at                  INTEGER NOT NULL,
    expires_at                  INTEGER,
    disabled                    INTEGER NOT NULL DEFAULT 0,
    pending_sync                INTEGER NOT NULL DEFAULT 1,
    created_by_admin_id         INTEGER REFERENCES admins(id),
    created_by_telegram_user_id INTEGER
);
CREATE INDEX idx_peers_pending_sync ON peers(pending_sync) WHERE pending_sync = 1;
CREATE INDEX idx_peers_disabled     ON peers(disabled);

CREATE TABLE telegram_users (
    telegram_user_id INTEGER PRIMARY KEY,
    username         TEXT,
    first_name       TEXT,
    quota_used       INTEGER NOT NULL DEFAULT 0,
    last_seen_at     INTEGER NOT NULL,
    is_member        INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE telegram_claims (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    telegram_user_id INTEGER NOT NULL REFERENCES telegram_users(telegram_user_id),
    peer_id          INTEGER NOT NULL REFERENCES peers(id),
    claimed_at       INTEGER NOT NULL
);
CREATE INDEX idx_telegram_claims_user ON telegram_claims(telegram_user_id);
