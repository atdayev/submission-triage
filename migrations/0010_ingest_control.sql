CREATE TABLE service_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- keyed on (uidvalidity, uid): a UID is unique only within its UIDVALIDITY
-- generation, so a mailbox reset must not quarantine unrelated messages
CREATE TABLE poll_failures (
    uidvalidity INTEGER NOT NULL,
    uid         INTEGER NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (uidvalidity, uid)
);

CREATE INDEX audit_created_idx ON audit_log(created_at);
CREATE INDEX outbox_status_created_idx ON outbox(status, created_at);
