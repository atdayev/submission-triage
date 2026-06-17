package service

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

// EscalationWorker periodically redelivers the outbox and runs escalation, closure, and digest checks.
type EscalationWorker struct {
	svc      *SubmissionsService
	interval time.Duration
	log      *logrus.Entry
}

// NewEscalationWorker returns a worker that runs periodic escalation checks.
func NewEscalationWorker(svc *SubmissionsService, interval time.Duration, log *logrus.Entry) *EscalationWorker {
	return &EscalationWorker{svc: svc, interval: interval, log: log}
}

// Run sweeps the outbox once, then ticks the periodic checks until ctx is canceled.
func (w *EscalationWorker) Run(ctx context.Context) {
	ctx = logger.ContextWithLogger(ctx, w.log)
	w.log.WithField("interval", w.interval.String()).Info("escalation worker started")
	// sweep the outbox immediately so post-crash replies don't wait a full interval
	if err := w.svc.RedeliverOutbox(ctx); err != nil {
		w.log.WithError(err).Error("startup outbox redelivery failed")
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	digestInterval := w.svc.cfg.Escalation.DigestInterval()
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
			if err := w.svc.RedeliverOutbox(ctx); err != nil {
				w.log.WithError(err).Error("outbox redelivery failed")
			}
			if err := w.svc.CheckEscalations(ctx); err != nil {
				w.log.WithError(err).Error("periodic escalation check failed")
			}
			if err := w.svc.CheckClosures(ctx); err != nil {
				w.log.WithError(err).Error("periodic closure check failed")
			}
		case <-digestC:
			if ctx.Err() != nil {
				return
			}
			if err := w.svc.SendEscalationDigest(ctx); err != nil {
				w.log.WithError(err).Error("escalation digest send failed")
			}
		}
	}
}
