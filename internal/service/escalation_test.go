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

func TestEscalationWorker_StopsOnContextCancel(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)

	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	log := logrus.NewEntry(logrus.New())
	svc := &SubmissionsService{
		cfg:        &config.Config{Escalation: config.EscalationConfig{ThresholdHours: 72}},
		repo:       &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
		checklists: &fakeStore{cl: model.Checklist{PolicyType: "cgl"}},
		now:        time.Now,
		log:        log,
	}

	w := NewEscalationWorker(svc, 30*time.Millisecond, log)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancel")
	}
}

func TestEscalationWorker_RecoversFromCheckPanic(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)

	var listStaleCalls atomic.Int32
	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ int64, _ int) ([]model.Submission, error) {
			if listStaleCalls.Add(1) == 1 {
				panic("poisoned row")
			}
			return nil, nil
		},
	)
	subs.On("ListCompletedBefore", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	var panicAudited atomic.Bool
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventWorkerPanic {
			panicAudited.Store(true)
		}
	}).Maybe()

	log := logrus.NewEntry(logrus.New())
	svc := &SubmissionsService{
		cfg:        &config.Config{Escalation: config.EscalationConfig{ThresholdHours: 72, AutoCloseAfterHours: 336}},
		repo:       &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
		checklists: &fakeStore{cl: model.Checklist{PolicyType: "cgl"}},
		now:        time.Now,
		log:        log,
	}

	w := NewEscalationWorker(svc, 15*time.Millisecond, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// the first tick panics inside CheckEscalations; a later tick must still run
	deadline := time.After(2 * time.Second)
	for listStaleCalls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("worker did not survive the panic; ListStale calls=%d", listStaleCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancel")
	}
	if !panicAudited.Load() {
		t.Error("expected worker.panic_recovered audit entry")
	}
}
