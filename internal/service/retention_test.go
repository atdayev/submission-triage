package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// retentionSvc builds a service wired only for the prune path.
func retentionSvc(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository,
	ob *repomocks.OutboxRepository, cfg config.RetentionConfig, now time.Time) *SubmissionsService {
	t.Helper()
	return &SubmissionsService{
		cfg:  &config.Config{Retention: cfg},
		repo: &repository.Repository{Submissions: subs, Audit: aud, Outbox: ob},
		now:  func() time.Time { return now },
		log:  logrus.NewEntry(logrus.New()),
	}
}

func TestPruneRetention_PrunesEveryTableOnItsHorizon(t *testing.T) {
	now := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	ob := repomocks.NewOutboxRepository(t)

	wantCutoff := now.Add(-90 * 24 * time.Hour).UnixNano()
	wantTTL := now.Add(-168 * time.Hour).UnixNano()

	ob.On("ExpirePending", mock.Anything, wantTTL).Return(int64(2), nil)
	aud.On("Prune", mock.Anything, wantCutoff).Return(int64(500), nil)
	ob.On("Prune", mock.Anything, wantCutoff).Return(int64(30), nil)
	subs.On("ClearExtractedTextForClosed", mock.Anything, wantCutoff).Return(int64(7), nil)

	svc := retentionSvc(t, subs, aud, ob, config.RetentionConfig{
		AuditDays: 90, OutboxPendingTTLHours: 168, IntervalHours: 24,
	}, now)

	touched, err := svc.PruneRetention(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if touched != 539 {
		t.Errorf("touched %d rows, want 539 (2 expired + 500 audit + 30 outbox + 7 documents)", touched)
	}
}

// Zero disables a sweep rather than pruning with a zero cutoff, which would delete
// everything ever written.
func TestPruneRetention_ZeroDisablesEachSweep(t *testing.T) {
	now := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	ob := repomocks.NewOutboxRepository(t)
	svc := retentionSvc(t, subs, aud, ob, config.RetentionConfig{}, now)

	touched, err := svc.PruneRetention(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if touched != 0 {
		t.Errorf("touched %d, want 0", touched)
	}
	ob.AssertNotCalled(t, "ExpirePending", mock.Anything, mock.Anything)
	aud.AssertNotCalled(t, "Prune", mock.Anything, mock.Anything)
	subs.AssertNotCalled(t, "ClearExtractedTextForClosed", mock.Anything, mock.Anything)
}

func TestPruneRetention_StopsOnError(t *testing.T) {
	now := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	ob := repomocks.NewOutboxRepository(t)

	ob.On("ExpirePending", mock.Anything, mock.Anything).Return(int64(0), nil)
	aud.On("Prune", mock.Anything, mock.Anything).Return(int64(0), errors.New("disk full"))

	svc := retentionSvc(t, subs, aud, ob, config.RetentionConfig{AuditDays: 90, OutboxPendingTTLHours: 168}, now)
	if _, err := svc.PruneRetention(context.Background()); err == nil {
		t.Fatal("expected the error to surface so the worker logs it")
	}
	// a failed audit prune must not cascade into dropping document text
	subs.AssertNotCalled(t, "ClearExtractedTextForClosed", mock.Anything, mock.Anything)
}

// VACUUM is expensive and needs free disk equal to the database; running it after a
// sweep that deleted nothing would be pure cost.
func TestRetentionWorker_VacuumsOnlyAfterDeletingSomething(t *testing.T) {
	now := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		pruned     int64
		wantVacuum bool
	}{
		{"nothing pruned", 0, false},
		{"rows pruned", 12, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs := repomocks.NewSubmissionRepository(t)
			aud := repomocks.NewAuditRepository(t)
			ob := repomocks.NewOutboxRepository(t)
			ob.On("ExpirePending", mock.Anything, mock.Anything).Return(int64(0), nil)
			aud.On("Prune", mock.Anything, mock.Anything).Return(tc.pruned, nil)
			ob.On("Prune", mock.Anything, mock.Anything).Return(int64(0), nil)
			subs.On("ClearExtractedTextForClosed", mock.Anything, mock.Anything).Return(int64(0), nil)

			svc := retentionSvc(t, subs, aud, ob, config.RetentionConfig{AuditDays: 90, OutboxPendingTTLHours: 168}, now)
			vacuumed := false
			w := NewRetentionWorker(svc, func(context.Context) error { vacuumed = true; return nil }, time.Hour, svc.log)
			w.sweep(context.Background())

			if vacuumed != tc.wantVacuum {
				t.Errorf("vacuumed=%v, want %v", vacuumed, tc.wantVacuum)
			}
		})
	}
}

// A failed VACUUM is survivable: the rows are already gone and the freed pages are
// reused, so the sweep must not be treated as failed.
func TestRetentionWorker_SurvivesVacuumFailure(t *testing.T) {
	now := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	ob := repomocks.NewOutboxRepository(t)
	ob.On("ExpirePending", mock.Anything, mock.Anything).Return(int64(0), nil)
	aud.On("Prune", mock.Anything, mock.Anything).Return(int64(5), nil)
	ob.On("Prune", mock.Anything, mock.Anything).Return(int64(0), nil)
	subs.On("ClearExtractedTextForClosed", mock.Anything, mock.Anything).Return(int64(0), nil)

	svc := retentionSvc(t, subs, aud, ob, config.RetentionConfig{AuditDays: 90, OutboxPendingTTLHours: 168}, now)
	w := NewRetentionWorker(svc, func(context.Context) error { return errors.New("no space left on device") }, time.Hour, svc.log)
	w.sweep(context.Background()) // must not panic or block
}

// Disabling the sweep is a supported configuration, but it must not silently look
// like a running worker.
func TestRetentionWorker_ZeroIntervalReturnsImmediately(t *testing.T) {
	svc := retentionSvc(t, repomocks.NewSubmissionRepository(t), repomocks.NewAuditRepository(t),
		repomocks.NewOutboxRepository(t), config.RetentionConfig{}, time.Now())
	done := make(chan struct{})
	go func() {
		NewRetentionWorker(svc, nil, 0, svc.log).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a zero interval must return, not block forever on a nil ticker")
	}
}
