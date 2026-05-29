package service

import (
	"context"
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
		repo:       &repository.Repository{Submissions: subs, Audit: aud},
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
