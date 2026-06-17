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

//go:generate mockery --name=AuditRepository --output=mocks --outpkg=mocks --filename=AuditRepository.go

// AuditRepository persists and queries the audit log.
type AuditRepository interface {
	Append(ctx context.Context, e *model.AuditEntry) error
	ListBySubmission(ctx context.Context, submissionID string) ([]model.AuditEntry, error)
}

// AuditRepositoryImpl is the SQLite-backed AuditRepository.
type AuditRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

// NewAuditRepository returns a SQLite-backed AuditRepository.
func NewAuditRepository(db *sql.DB, log *logrus.Entry) *AuditRepositoryImpl {
	return &AuditRepositoryImpl{db: db, log: log}
}

// Append writes one audit entry, defaulting id and timestamp.
func (r *AuditRepositoryImpl) Append(ctx context.Context, e *model.AuditEntry) error {
	if e == nil {
		return fmt.Errorf("audit: nil entry")
	}
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("audit: marshal payload: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, submission_id, event_type, payload, request_id, created_at)
		VALUES (?,?,?,?,?,?)`,
		e.ID, e.SubmissionID, string(e.EventType), string(payload), e.RequestID, e.CreatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// ListBySubmission returns a submission's audit entries, oldest first.
func (r *AuditRepositoryImpl) ListBySubmission(ctx context.Context, submissionID string) ([]model.AuditEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, submission_id, event_type, payload, request_id, created_at
		FROM audit_log WHERE submission_id = ?
		ORDER BY created_at ASC`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("audit: query by submission: %w", err)
	}
	defer rows.Close()
	return r.scanAuditRows(rows)
}

func (r *AuditRepositoryImpl) scanAuditRows(rows *sql.Rows) ([]model.AuditEntry, error) {
	var out []model.AuditEntry
	for rows.Next() {
		var (
			e         model.AuditEntry
			eventType string
			payload   string
			created   int64
		)
		if err := rows.Scan(&e.ID, &e.SubmissionID, &eventType, &payload, &e.RequestID, &created); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		e.EventType = model.EventType(eventType)
		e.CreatedAt = time.Unix(0, created).UTC()
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil && r.log != nil {
				r.log.WithError(err).WithField("audit_id", e.ID).Warn("audit payload unmarshal failed")
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
