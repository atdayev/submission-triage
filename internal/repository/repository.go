package repository

import (
	"database/sql"

	"github.com/sirupsen/logrus"
)

type Repository struct {
	Submissions SubmissionRepository
	Audit       AuditRepository
	Outbox      OutboxRepository
}

func NewRepository(db *sql.DB, log *logrus.Entry) *Repository {
	return &Repository{
		Submissions: NewSubmissionRepository(db, log),
		Audit:       NewAuditRepository(db, log),
		Outbox:      NewOutboxRepository(db, log),
	}
}
