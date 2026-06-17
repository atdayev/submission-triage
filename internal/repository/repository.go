package repository

import (
	"database/sql"

	"github.com/sirupsen/logrus"
)

// Repository groups the submission, audit, and outbox repositories.
type Repository struct {
	Submissions SubmissionRepository
	Audit       AuditRepository
	Outbox      OutboxRepository
}

// NewRepository wires the repositories over a shared database.
func NewRepository(db *sql.DB, log *logrus.Entry) *Repository {
	return &Repository{
		Submissions: NewSubmissionRepository(db, log),
		Audit:       NewAuditRepository(db, log),
		Outbox:      NewOutboxRepository(db, log),
	}
}
