//go:build stress

// Real-database harness for the stress suite: a live migrated SQLite file instead of
// repository mocks.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/database"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
)

// countingSender records every send and mints a unique provider Message-ID, the way
// a real SMTP server does. A fixed id would collapse distinct sends into one
// outbound row and hide exactly the double-send this suite is looking for.
type countingSender struct {
	mu    sync.Mutex
	sent  []model.Reply
	seq   atomic.Int64
	delay time.Duration
}

func (s *countingSender) Name() string { return "stress" }

func (s *countingSender) SendThreadedReply(ctx context.Context, r model.Reply) (string, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	n := s.seq.Add(1)
	s.mu.Lock()
	s.sent = append(s.sent, r)
	s.mu.Unlock()
	return fmt.Sprintf("stress-%d@test", n), nil
}

func (s *countingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// countBySubmission reports how many replies each submission actually received.
func (s *countingSender) countBySubmission() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, r := range s.sent {
		out[r.SubmissionID]++
	}
	return out
}

// realHarness is a service over a real migrated SQLite database.
type realHarness struct {
	t    *testing.T
	svc  *SubmissionsService
	repo *repository.Repository
	mail *countingSender
	db   *sql.DB
}

// count runs a single-value aggregate query, failing the test on error.
func (h *realHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// dbQueryRow scans one value out of the database.
func (h *realHarness) dbQueryRow(query string, dest any) error {
	return h.db.QueryRow(query).Scan(dest)
}

func newRealHarness(t *testing.T, cfg *config.Config) *realHarness {
	t.Helper()
	cl := smallChecklist()
	log := logrus.NewEntry(logrus.New())
	log.Logger.SetLevel(logrus.ErrorLevel) // a volume run must not drown the output

	path := filepath.Join(t.TempDir(), "real.db")
	db, err := database.Open(context.Background(), path, log)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(context.Background(), db, filepath.Join("..", "..", "migrations"), log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := repository.NewRepository(db, log)
	mail := &countingSender{}
	svc := NewSubmissionsService(Dependencies{
		Config:         cfg,
		Repository:     repo,
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: &fakeStore{cl: cl},
		TextExtractors: map[string]TextExtractor{"application/pdf": fakeExtractor{}},
		Log:            log,
	})
	t.Cleanup(svc.Shutdown)

	return &realHarness{t: t, svc: svc, repo: repo, mail: mail, db: db}
}

func realCfg() *config.Config {
	return &config.Config{
		Escalation: config.EscalationConfig{
			ThresholdHours: 72, AutoCloseAfterHours: 336, AwaitingAutoCloseAfterHours: 2160,
			BindWindowDays: 7, BindGraceDays: 3, MinGapHours: 24,
		},
		Classifier: config.ClassifierConfig{ConfidenceFloor: 0.80},
		Document:   config.DocumentConfig{UnreadableMinSizeBytes: 51200, UnreadableMinChars: 100},
		Retention:  config.RetentionConfig{AuditDays: 90, OutboxPendingTTLHours: 168, IntervalHours: 24},
	}
}

func submissionReq(id, subject string) IngestRequest {
	return IngestRequest{
		MessageID:   id,
		FromAddress: "broker@example.com",
		Subject:     subject,
		Attachments: []model.Attachment{{
			Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125 " + id),
		}},
	}
}
