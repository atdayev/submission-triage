package service

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

// PruneRetention bounds what the database keeps: audit rows and settled outbox rows
// past the retention horizon, the extracted text of long-closed submissions, and
// pending replies that never came due. Nothing else deletes anything, so without
// this sweep the file only ever grows. Reports how many rows it touched.
func (s *SubmissionsService) PruneRetention(ctx context.Context) (int64, error) {
	now := s.now()
	var touched int64

	// a pending row whose not_before never arrived is never handed to the sender, so
	// its attempt count never rises and it can never dead-letter on its own — it just
	// holds its submission's one-pending slot forever
	if ttl := s.cfg.Retention.OutboxPendingTTL(); ttl > 0 {
		n, err := s.repo.Outbox.ExpirePending(ctx, now.Add(-ttl).UnixNano())
		if err != nil {
			return touched, err
		}
		if n > 0 {
			s.log.WithField("count", n).Warn("retention: expired pending replies past their TTL")
		}
		touched += n
	}

	window := s.cfg.Retention.AuditRetention()
	if window <= 0 {
		return touched, nil
	}
	cutoff := now.Add(-window).UnixNano()

	audits, err := s.repo.Audit.Prune(ctx, cutoff)
	if err != nil {
		return touched, err
	}
	settled, err := s.repo.Outbox.Prune(ctx, cutoff)
	if err != nil {
		return touched, err
	}
	docs, err := s.repo.Submissions.ClearExtractedTextForClosed(ctx, cutoff)
	if err != nil {
		return touched, err
	}
	touched += audits + settled + docs
	if touched > 0 {
		s.log.WithFields(logrus.Fields{
			"audit_rows":  audits,
			"outbox_rows": settled,
			"documents":   docs,
		}).Info("retention sweep complete")
	}
	return touched, nil
}

// RetentionWorker periodically prunes the database and reclaims its free pages.
type RetentionWorker struct {
	svc      *SubmissionsService
	vacuum   func(context.Context) error
	interval time.Duration
	log      *logrus.Entry
}

// NewRetentionWorker returns a worker that prunes on interval, vacuuming after any
// sweep that actually deleted something.
func NewRetentionWorker(svc *SubmissionsService, vacuum func(context.Context) error, interval time.Duration, log *logrus.Entry) *RetentionWorker {
	return &RetentionWorker{svc: svc, vacuum: vacuum, interval: interval, log: log}
}

// Run prunes at startup and then on interval, until ctx is canceled.
func (w *RetentionWorker) Run(ctx context.Context) {
	if w.interval <= 0 {
		w.log.Warn("retention sweep disabled; the database will grow without bound")
		return
	}
	ctx = logger.ContextWithLogger(ctx, w.log)
	w.log.WithField("interval", w.interval.String()).Info("retention worker started")
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("retention worker stopping")
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			recoverCheck(ctx, w.svc, w.log, "retention", func() { w.sweep(ctx) })
		}
	}
}

func (w *RetentionWorker) sweep(ctx context.Context) {
	touched, err := w.svc.PruneRetention(ctx)
	if err != nil {
		w.log.WithError(err).Error("retention sweep failed")
		return
	}
	if touched == 0 || w.vacuum == nil {
		return
	}
	// VACUUM needs free space roughly equal to the database; on a full disk it fails
	// and the pruned pages stay free-but-unreclaimed, which is still bounded
	if err := w.vacuum(ctx); err != nil {
		w.log.WithError(err).Warn("vacuum failed; freed pages stay allocated but are reused")
	}
}
