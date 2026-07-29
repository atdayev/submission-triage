package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

//go:generate mockery --name=IngestControlRepository --output=mocks --outpkg=mocks --filename=IngestControlRepository.go

// firstBootKey stores the timestamp of the first successful boot, the fallback
// ingest cutoff when IMAP_IGNORE_BEFORE is unset.
const firstBootKey = "first_boot_at"

// IngestControlRepository holds the poller's durable state: the ingest cutoff and
// the per-message failure counts that quarantine a poisoned message.
type IngestControlRepository interface {
	// FirstBootAt returns the persisted first-boot timestamp, setting it to now on
	// first call. It must be persisted, not derived at boot: a restart after a week
	// of downtime would otherwise skip a week of real mail.
	FirstBootAt(ctx context.Context, now time.Time) (time.Time, error)
	// RecordFailure increments a message's failure count and returns the new total.
	RecordFailure(ctx context.Context, uidValidity, uid uint32, reason string, now time.Time) (int, error)
	// ClearFailure forgets a message that finally succeeded.
	ClearFailure(ctx context.Context, uidValidity, uid uint32) error
}

type IngestControlRepositoryImpl struct {
	db  *sql.DB
	log *logrus.Entry
}

func NewIngestControlRepository(db *sql.DB, log *logrus.Entry) *IngestControlRepositoryImpl {
	return &IngestControlRepositoryImpl{db: db, log: log}
}

// FirstBootAt reads the stored first-boot timestamp, writing now if absent.
func (r *IngestControlRepositoryImpl) FirstBootAt(ctx context.Context, now time.Time) (time.Time, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM service_meta WHERE key = ?`, firstBootKey).Scan(&raw)
	switch {
	case err == nil:
		t, perr := time.Parse(time.RFC3339Nano, raw)
		if perr != nil {
			return time.Time{}, fmt.Errorf("service_meta: %s %q unparseable: %w", firstBootKey, raw, perr)
		}
		return t.UTC(), nil
	case !errors.Is(err, sql.ErrNoRows):
		return time.Time{}, fmt.Errorf("service_meta: read %s: %w", firstBootKey, err)
	}
	stamped := now.UTC()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO service_meta (key, value) VALUES (?,?) ON CONFLICT(key) DO NOTHING`,
		firstBootKey, stamped.Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, fmt.Errorf("service_meta: write %s: %w", firstBootKey, err)
	}
	return stamped, nil
}

// RecordFailure increments a message's failure count and returns the new total.
func (r *IngestControlRepositoryImpl) RecordFailure(ctx context.Context, uidValidity, uid uint32, reason string, now time.Time) (int, error) {
	var attempts int
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO poll_failures (uidvalidity, uid, attempts, last_error, updated_at)
		VALUES (?,?,1,?,?)
		ON CONFLICT(uidvalidity, uid) DO UPDATE SET
			attempts = poll_failures.attempts + 1,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
		RETURNING attempts`,
		uidValidity, uid, reason, now.UTC().UnixNano(),
	).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("poll_failures: record: %w", err)
	}
	return attempts, nil
}

// ClearFailure removes a message's failure row.
func (r *IngestControlRepositoryImpl) ClearFailure(ctx context.Context, uidValidity, uid uint32) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM poll_failures WHERE uidvalidity = ? AND uid = ?`, uidValidity, uid); err != nil {
		return fmt.Errorf("poll_failures: clear: %w", err)
	}
	return nil
}
