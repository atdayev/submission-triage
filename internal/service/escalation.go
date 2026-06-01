package service

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

type EscalationWorker struct {
	svc      *SubmissionsService
	interval time.Duration
	log      *logrus.Entry
}

func NewEscalationWorker(svc *SubmissionsService, interval time.Duration, log *logrus.Entry) *EscalationWorker {
	return &EscalationWorker{svc: svc, interval: interval, log: log}
}

func (w *EscalationWorker) Run(ctx context.Context) {
	ctx = logger.ContextWithLogger(ctx, w.log)
	w.log.WithField("interval", w.interval.String()).Info("escalation worker started")
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
			if err := w.svc.SendEscalationDigest(ctx); err != nil {
				w.log.WithError(err).Error("escalation digest send failed")
			}
		}
	}
}
