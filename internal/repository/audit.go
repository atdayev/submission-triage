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
	// ListSubmissionIDsByEvent returns the distinct submission ids carrying an event
	// of this type recorded at or after since.
	ListSubmissionIDsByEvent(ctx context.Context, eventType model.EventType, sinceUnixNano int64) ([]string, error)
	// Prune deletes entries older than the cutoff, returning how many went. Nothing
	// else deletes from audit_log, so without this the table grows forever.
	Prune(ctx context.Context, olderThanUnixNano int64) (int64, error)
}

type AuditRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

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

// ListSubmissionIDsByEvent returns the distinct submission ids with an event of
// this type at or after since.
func (r *AuditRepositoryImpl) ListSubmissionIDsByEvent(ctx context.Context, eventType model.EventType, sinceUnixNano int64) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT submission_id FROM audit_log
		WHERE event_type = ? AND created_at >= ? AND submission_id != ''`,
		string(eventType), sinceUnixNano)
	if err != nil {
		return nil, fmt.Errorf("audit: query by event: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows, "audit submission id")
}

// Prune deletes entries older than the cutoff.
func (r *AuditRepositoryImpl) Prune(ctx context.Context, olderThanUnixNano int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM audit_log WHERE created_at < ?`, olderThanUnixNano)
	if err != nil {
		return 0, fmt.Errorf("audit: prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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
