CREATE TABLE IF NOT EXISTS submissions (
    id              TEXT PRIMARY KEY,
    policy_type     TEXT NOT NULL,
    state           TEXT NOT NULL,
    subject_line    TEXT NOT NULL DEFAULT '',
    from_address    TEXT NOT NULL DEFAULT '',
    from_name       TEXT NOT NULL DEFAULT '',
    thread_key      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    last_action_at  INTEGER NOT NULL,
    escalated_at    INTEGER,
    missing_items   TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS submissions_state_idx ON submissions(state);
CREATE INDEX IF NOT EXISTS submissions_last_action_idx ON submissions(state, last_action_at);

CREATE TABLE IF NOT EXISTS emails (
    deterministic_id TEXT PRIMARY KEY,
    submission_id    TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    direction        TEXT NOT NULL,
    message_id       TEXT NOT NULL DEFAULT '',
    in_reply_to      TEXT NOT NULL DEFAULT '',
    refs             TEXT NOT NULL DEFAULT '[]',
    from_address     TEXT NOT NULL DEFAULT '',
    from_name        TEXT NOT NULL DEFAULT '',
    to_addresses     TEXT NOT NULL DEFAULT '[]',
    subject          TEXT NOT NULL DEFAULT '',
    body_text        TEXT NOT NULL DEFAULT '',
    received_at      INTEGER NOT NULL,
    provider_msg_id  TEXT NOT NULL DEFAULT '',
    attachments      TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS emails_submission_idx ON emails(submission_id);
CREATE INDEX IF NOT EXISTS emails_message_id_idx ON emails(message_id);
CREATE INDEX IF NOT EXISTS emails_in_reply_to_idx ON emails(in_reply_to);

CREATE TABLE IF NOT EXISTS documents (
    id               TEXT PRIMARY KEY,
    submission_id    TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    email_id         TEXT NOT NULL DEFAULT '',
    filename         TEXT NOT NULL DEFAULT '',
    content_type     TEXT NOT NULL DEFAULT '',
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    sha256           TEXT NOT NULL DEFAULT '',
    classified_as    TEXT NOT NULL DEFAULT '',
    confidence       REAL NOT NULL DEFAULT 0,
    classified_by    TEXT NOT NULL DEFAULT '',
    extracted_text   TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS documents_submission_idx ON documents(submission_id);

CREATE TABLE IF NOT EXISTS audit_log (
    id            TEXT PRIMARY KEY,
    submission_id TEXT NOT NULL DEFAULT '',
    event_type    TEXT NOT NULL,
    payload       TEXT NOT NULL DEFAULT '{}',
    request_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS audit_submission_idx ON audit_log(submission_id, created_at);
CREATE INDEX IF NOT EXISTS audit_event_idx ON audit_log(event_type, created_at);
