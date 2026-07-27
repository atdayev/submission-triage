package service

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

// OutboxWorker is the sole owner of the outbox sweep. It sends replies whose
// coalesce window has elapsed and retries failed sends, on a short interval so
// reply latency tracks the coalesce window rather than the escalation interval.
type OutboxWorker struct {
	svc      *SubmissionsService
	interval time.Duration
	log      *logrus.Entry
}

// NewOutboxWorker returns a worker that sweeps the outbox every interval.
func NewOutboxWorker(svc *SubmissionsService, interval time.Duration, log *logrus.Entry) *OutboxWorker {
	return &OutboxWorker{svc: svc, interval: interval, log: log}
}

// Run sweeps once at startup (post-crash replies don't wait a full interval),
// then every interval until ctx is canceled.
func (w *OutboxWorker) Run(ctx context.Context) {
	ctx = logger.ContextWithLogger(ctx, w.log)
	w.log.WithField("interval", w.interval.String()).Info("outbox worker started")
	w.sweep(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox worker stopping")
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			w.sweep(ctx)
		}
	}
}

func (w *OutboxWorker) sweep(ctx context.Context) {
	recoverCheck(ctx, w.svc, w.log, "outbox_sweep", func() {
		if err := w.svc.RedeliverOutbox(ctx); err != nil {
			w.log.WithError(err).Error("outbox redelivery failed")
		}
	})
}
