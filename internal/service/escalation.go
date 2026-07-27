package service

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/logger"
)

// recoverCheck runs a worker's check body, recovering from a panic so a poisoned
// row can't kill the goroutine (and likely the process). It wraps the body only,
// never the surrounding select, so context cancellation is never swallowed.
func recoverCheck(ctx context.Context, svc *SubmissionsService, log *logrus.Entry, name string, body func()) {
	defer func() {
		if r := recover(); r != nil {
			log.WithField("check", name).Errorf("worker recovered from panic: %v\n%s", r, debug.Stack())
			svc.audit(ctx, "", model.EventWorkerPanic, map[string]any{"check": name})
		}
	}()
	body()
}

// EscalationWorker periodically runs escalation, closure, and digest checks.
type EscalationWorker struct {
	svc      *SubmissionsService
	interval time.Duration
	log      *logrus.Entry
}

// NewEscalationWorker returns a worker that runs periodic escalation checks.
func NewEscalationWorker(svc *SubmissionsService, interval time.Duration, log *logrus.Entry) *EscalationWorker {
	return &EscalationWorker{svc: svc, interval: interval, log: log}
}

// Run ticks the periodic checks until ctx is canceled. The outbox sweep is not
// here — the OutboxWorker owns it so reply latency isn't tied to this interval.
func (w *EscalationWorker) Run(ctx context.Context) {
	ctx = logger.ContextWithLogger(ctx, w.log)
	w.log.WithField("interval", w.interval.String()).Info("escalation worker started")
	t := time.NewTicker(w.interval)
	defer t.Stop()
	digestInterval := w.svc.cfg.Digest.Interval()
	var digestTimer *time.Ticker
	var digestC <-chan time.Time
	if digestInterval > 0 {
		digestTimer = time.NewTicker(digestInterval)
		defer digestTimer.Stop()
		digestC = digestTimer.C
	}
	for {
		select {
		case <-ctx.Done():
			w.log.Info("escalation worker stopping")
			return
		case <-t.C:
			if ctx.Err() != nil {
				return // canceled concurrently with the tick; skip a doomed cycle
			}
			recoverCheck(ctx, w.svc, w.log, "periodic_checks", func() {
				if err := w.svc.CheckEscalations(ctx); err != nil {
					w.log.WithError(err).Error("periodic escalation check failed")
				}
				if err := w.svc.CheckClosures(ctx); err != nil {
					w.log.WithError(err).Error("periodic closure check failed")
				}
			})
		case <-digestC:
			if ctx.Err() != nil {
				return
			}
			recoverCheck(ctx, w.svc, w.log, "digest", func() {
				if err := w.svc.SendDigest(ctx); err != nil {
					w.log.WithError(err).Error("digest send failed")
				}
			})
		}
	}
}
