ALTER TABLE submissions ADD COLUMN needs_review INTEGER NOT NULL DEFAULT 0;
UPDATE submissions SET needs_review = 1, state = 'awaiting' WHERE state = 'needs_review';
