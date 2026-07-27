ALTER TABLE submissions ADD COLUMN last_reply_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE outbox ADD COLUMN not_before INTEGER NOT NULL DEFAULT 0;

-- collapse any pre-existing multiple pending rows to the newest per submission
-- so the one-pending-per-submission unique index below can be created safely
DELETE FROM outbox WHERE status = 'pending' AND rowid NOT IN (
    SELECT MAX(rowid) FROM outbox WHERE status = 'pending' GROUP BY submission_id
);

DROP INDEX IF EXISTS outbox_status_idx;
CREATE INDEX IF NOT EXISTS outbox_status_not_before_idx ON outbox(status, not_before);

CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_one_pending ON outbox(submission_id) WHERE status = 'pending';
