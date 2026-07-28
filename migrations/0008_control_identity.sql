ALTER TABLE submissions ADD COLUMN named_insured   TEXT    NOT NULL DEFAULT '';
ALTER TABLE submissions ADD COLUMN effective_date  INTEGER;
ALTER TABLE submissions ADD COLUMN on_hold           INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN delivery_failed   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN last_escalated_at INTEGER;
ALTER TABLE documents   ADD COLUMN encrypted       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE emails      ADD COLUMN reply_to         TEXT    NOT NULL DEFAULT '';

CREATE INDEX submissions_effective_idx ON submissions(state, effective_date);
CREATE INDEX submissions_insured_idx   ON submissions(named_insured, from_address);
