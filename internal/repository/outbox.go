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
type OutboxRepository interface {
	Enqueue(ctx context.Context, e *model.OutboxEntry) error
	// ListPending returns pending entries older than olderThan (so a row the
	// online sender is still handling isn't swept), oldest first.
	ListPending(ctx context.Context, olderThan time.Time, limit int) ([]model.OutboxEntry, error)
	Update(ctx context.Context, id string, status model.OutboxStatus, attempts int, lastErr string) error
}

type OutboxRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

func NewOutboxRepository(db *sql.DB, log *logrus.Entry) *OutboxRepositoryImpl {
	return &OutboxRepositoryImpl{db: db, log: log}
}

func (r *OutboxRepositoryImpl) Enqueue(ctx context.Context, e *model.OutboxEntry) error {
	return insertOutboxRow(ctx, r.db, e)
}

// execContext lets insertOutboxRow run on a *sql.DB or a caller's *sql.Tx.
type execContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertOutboxRow writes one pending reply, defaulting id/timestamps/status.
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
	_, err = ex.ExecContext(ctx, `
		INSERT INTO outbox (id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		e.ID, e.SubmissionID, string(payload), string(e.Status), e.Attempts, e.LastError,
		e.CreatedAt.UnixNano(), e.UpdatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

func (r *OutboxRepositoryImpl) ListPending(ctx context.Context, olderThan time.Time, limit int) ([]model.OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at
		FROM outbox WHERE status = ? AND created_at <= ?
		ORDER BY created_at ASC LIMIT ?`,
		string(model.OutboxPending), olderThan.UnixNano(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: query pending: %w", err)
	}
	defer rows.Close()

	var out []model.OutboxEntry
	for rows.Next() {
		var (
			e                 model.OutboxEntry
			replyJSON, status string
			created, updated  int64
		)
		if err := rows.Scan(&e.ID, &e.SubmissionID, &replyJSON, &status, &e.Attempts, &e.LastError, &created, &updated); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		if err := json.Unmarshal([]byte(replyJSON), &e.Reply); err != nil {
			return nil, fmt.Errorf("outbox: unmarshal reply %s: %w", e.ID, err)
		}
		e.Status = model.OutboxStatus(status)
		e.CreatedAt = time.Unix(0, created).UTC()
		e.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

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
