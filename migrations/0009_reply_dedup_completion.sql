ALTER TABLE submissions ADD COLUMN last_reply_hash     TEXT    NOT NULL DEFAULT '';
ALTER TABLE submissions ADD COLUMN completed_at        INTEGER;
ALTER TABLE submissions ADD COLUMN policy_clarify_asks INTEGER NOT NULL DEFAULT 0;

CREATE INDEX submissions_completed_idx ON submissions(state, completed_at);

-- existing complete rows keep their close clock rather than restarting it
UPDATE submissions SET completed_at = updated_at WHERE state = 'complete';
