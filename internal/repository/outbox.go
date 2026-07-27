package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

//go:generate mockery --name=OutboxRepository --output=mocks --outpkg=mocks --filename=OutboxRepository.go

// OutboxRepository persists outbound replies awaiting delivery.
type OutboxRepository interface {
	Enqueue(ctx context.Context, e *model.OutboxEntry) error
	// ListPending returns due pending entries, earliest-scheduled first: those
	// whose coalesce window has elapsed (not_before <= now) and, for already-
	// attempted rows only, whose retry backoff has elapsed (updated_at <=
	// retryCutoff). A never-attempted row is due as soon as not_before passes.
	ListPending(ctx context.Context, now, retryCutoff time.Time, limit int) ([]model.OutboxEntry, error)
	Update(ctx context.Context, id string, status model.OutboxStatus, attempts int, lastErr string) error
	// MarkSent marks a pending row sent only if it hasn't been superseded since
	// version (the updated_at observed when the reply was read for sending). If a
	// follow-up coalesced into the row in the meantime (updated_at advanced), the
	// mark no-ops and the newer reply stays pending for the sweeper — so a
	// coalesced reply is never lost to a stale mark-sent.
	MarkSent(ctx context.Context, id string, version time.Time) error
}

// OutboxRepositoryImpl is the SQLite-backed OutboxRepository.
type OutboxRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

// NewOutboxRepository returns a SQLite-backed OutboxRepository.
func NewOutboxRepository(db *sql.DB, log *logrus.Entry) *OutboxRepositoryImpl {
	return &OutboxRepositoryImpl{db: db, log: log}
}

// Enqueue persists a pending outbound reply.
func (r *OutboxRepositoryImpl) Enqueue(ctx context.Context, e *model.OutboxEntry) error {
	return insertOutboxRow(ctx, r.db, e)
}

// execContext lets insertOutboxRow run on a *sql.DB or a caller's *sql.Tx.
type execContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// insertOutboxRow upserts the single pending reply for a submission. A second
// pending reply supersedes the first (one-pending-per-submission), so rapid
// follow-ups coalesce. e.ID is set to the surviving row's id via RETURNING, so
// the online dispatcher always marks the real row.
func insertOutboxRow(ctx context.Context, ex execContext, e *model.OutboxEntry) error {
	if e == nil {
		return fmt.Errorf("outbox: nil entry")
	}
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.Status == "" {
		e.Status = model.OutboxPending
	}
	payload, err := json.Marshal(e.Reply)
	if err != nil {
		return fmt.Errorf("outbox: marshal reply: %w", err)
	}
	// on conflict keep attempts/created_at (a superseding reply inherits the
	// failure count so it can still dead-letter); overwrite content + schedule
	err = ex.QueryRowContext(ctx, `
		INSERT INTO outbox (id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at, not_before)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(submission_id) WHERE status = 'pending' DO UPDATE SET
			reply_json = excluded.reply_json,
			not_before = excluded.not_before,
			updated_at = excluded.updated_at
		RETURNING id`,
		e.ID, e.SubmissionID, string(payload), string(e.Status), e.Attempts, e.LastError,
		e.CreatedAt.UnixNano(), e.UpdatedAt.UnixNano(), nanoOrZero(e.NotBefore),
	).Scan(&e.ID)
	if err != nil {
		return fmt.Errorf("outbox: upsert: %w", err)
	}
	return nil
}

// ListPending returns due pending entries, earliest-scheduled first.
func (r *OutboxRepositoryImpl) ListPending(ctx context.Context, now, retryCutoff time.Time, limit int) ([]model.OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at, not_before
		FROM outbox
		WHERE status = ? AND not_before <= ? AND (attempts = 0 OR updated_at <= ?)
		ORDER BY not_before ASC LIMIT ?`,
		string(model.OutboxPending), now.UnixNano(), retryCutoff.UnixNano(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: query pending: %w", err)
	}
	defer rows.Close()

	var out, poison []model.OutboxEntry
	for rows.Next() {
		var (
			e                           model.OutboxEntry
			replyJSON, status           string
			created, updated, notBefore int64
		)
		if err := rows.Scan(&e.ID, &e.SubmissionID, &replyJSON, &status, &e.Attempts, &e.LastError, &created, &updated, &notBefore); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		e.Status = model.OutboxStatus(status)
		e.CreatedAt = time.Unix(0, created).UTC()
		e.UpdatedAt = time.Unix(0, updated).UTC()
		if notBefore != 0 {
			e.NotBefore = time.Unix(0, notBefore).UTC()
		}
		if err := json.Unmarshal([]byte(replyJSON), &e.Reply); err != nil {
			r.log.WithError(err).WithField("outbox_id", e.ID).Warn("outbox: undecodable reply; dead-lettering")
			poison = append(poison, e)
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("outbox: rows: %w", err)
	}
	// one undecodable row must not head-of-line block the queue
	for _, e := range poison {
		if err := r.Update(ctx, e.ID, model.OutboxFailed, e.Attempts+1, "undecodable reply_json"); err != nil {
			r.log.WithError(err).WithField("outbox_id", e.ID).Warn("outbox: dead-letter failed")
		}
	}
	return out, nil
}

// MarkSent marks a pending row sent iff its version (updated_at) is unchanged
// since it was read; a superseded row (coalesced follow-up) is left pending.
func (r *OutboxRepositoryImpl) MarkSent(ctx context.Context, id string, version time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE outbox SET status = ?, updated_at = ?
		WHERE id = ? AND status = ? AND updated_at = ?`,
		string(model.OutboxSent), time.Now().UTC().UnixNano(),
		id, string(model.OutboxPending), version.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("outbox: mark sent: %w", err)
	}
	return nil
}

// Update sets the status, attempt count, and last error of an outbox entry.
func (r *OutboxRepositoryImpl) Update(ctx context.Context, id string, status model.OutboxStatus, attempts int, lastErr string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE outbox SET status = ?, attempts = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(status), attempts, lastErr, time.Now().UTC().UnixNano(), id,
	)
	if err != nil {
		return fmt.Errorf("outbox: update: %w", err)
	}
	return nil
}
