package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

func outboxWorkerSvc(obm *repomocks.OutboxRepository, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository) *SubmissionsService {
	return &SubmissionsService{
		cfg:               &config.Config{},
		repo:              &repository.Repository{Submissions: subs, Audit: aud, Outbox: obm},
		now:               time.Now,
		log:               logrus.NewEntry(logrus.New()),
		outboxRetryAfter:  0,
		outboxMaxAttempts: 3,
		outboxBatch:       100,
	}
}

func TestOutboxWorker_SweepsAtStartupAndStopsOnCancel(t *testing.T) {
	obm := repomocks.NewOutboxRepository(t)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)

	var sweeps atomic.Int32
	obm.On("ListPending", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]model.OutboxEntry(nil), nil).
		Run(func(mock.Arguments) { sweeps.Add(1) })

	svc := outboxWorkerSvc(obm, subs, aud)
	log := logrus.NewEntry(logrus.New())
	w := NewOutboxWorker(svc, time.Hour, log) // long interval: only the startup sweep should fire

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for sweeps.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("startup sweep did not fire before the first tick")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox worker did not stop on context cancel")
	}
	if sweeps.Load() != 1 {
		t.Errorf("with a 1h interval exactly one (startup) sweep should run, got %d", sweeps.Load())
	}
}

func TestOutboxWorker_RecoversFromSweepPanic(t *testing.T) {
	obm := repomocks.NewOutboxRepository(t)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)

	var calls atomic.Int32
	obm.On("ListPending", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _, _ time.Time, _ int) ([]model.OutboxEntry, error) {
			if calls.Add(1) == 1 {
				panic("poisoned sweep")
			}
			return nil, nil
		},
	)
	var panicAudited atomic.Bool
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventWorkerPanic {
			panicAudited.Store(true)
		}
	}).Maybe()

	svc := outboxWorkerSvc(obm, subs, aud)
	log := logrus.NewEntry(logrus.New())
	w := NewOutboxWorker(svc, 15*time.Millisecond, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for calls.Load() < 2 { // survived the panic and swept again
		select {
		case <-deadline:
			t.Fatalf("worker did not survive the sweep panic; calls=%d", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox worker did not stop after cancel")
	}
	if !panicAudited.Load() {
		t.Error("expected a worker.panic_recovered audit entry")
	}
}
