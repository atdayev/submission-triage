package imap

import (
	"bytes"
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/delivery/emailingest"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/emlparse"
	"github.com/atdayev/submission-triage/pkg/logger"
)

const defaultBatchLimit = 50

// rawMessage is one fetched message: its UID plus the raw RFC822 bytes.
type rawMessage struct {
	UID uint32
	Raw []byte
}

// mailbox is what the poller needs from IMAP; client.go has the real adapter.
type mailbox interface {
	FetchUnseen(ctx context.Context, limit int) ([]rawMessage, error)
	MarkSeen(ctx context.Context, uid uint32) error
	Close() error
}

// ingester is what the poller needs from the service.
type ingester interface {
	IngestEmail(ctx context.Context, req service.IngestRequest) (service.IngestResult, error)
}

type Poller struct {
	dial       func(ctx context.Context) (mailbox, error)
	ingest     ingester
	interval   time.Duration
	batchLimit int
	mailbox    string
	log        *logrus.Entry
}

func NewPoller(cfg config.IMAPConfig, svc ingester, log *logrus.Entry) *Poller {
	return &Poller{
		dial:       dialIMAP(cfg, log),
		ingest:     svc,
		interval:   cfg.PollInterval(),
		batchLimit: defaultBatchLimit,
		mailbox:    cfg.Mailbox,
		log:        log,
	}
}

func (p *Poller) Run(ctx context.Context) {
	p.log.WithFields(logrus.Fields{
		"interval": p.interval.String(),
		"mailbox":  p.mailbox,
	}).Info("imap poller started")

	p.pollOnce(ctx) // catch up at startup rather than waiting a full interval
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("imap poller stopping")
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	mb, err := p.dial(ctx)
	if err != nil {
		p.log.WithError(err).Warn("imap connect failed; will retry next tick")
		return
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(ctx, p.batchLimit)
	if err != nil {
		p.log.WithError(err).Warn("imap fetch failed")
		return
	}
	if len(msgs) == 0 {
		p.log.Debug("imap poll: no unseen messages")
		return
	}
	p.log.WithField("count", len(msgs)).Info("imap poll: processing unseen messages")
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		p.process(ctx, mb, m)
	}
}

func (p *Poller) process(ctx context.Context, mb mailbox, m rawMessage) {
	rid := logger.GenerateRequestID()
	log := p.log.WithFields(logrus.Fields{logger.RequestIDField: rid, "uid": m.UID})
	ctx = logger.ContextWithRequestID(logger.ContextWithLogger(ctx, log), rid)

	payload, err := emlparse.FromReader(bytes.NewReader(m.Raw))
	if err != nil {
		log.WithError(err).Warn("imap: unparseable message; marking read")
		_ = mb.MarkSeen(ctx, m.UID)
		return
	}

	res, err := p.ingest.IngestEmail(ctx, emailingest.Translate(payload, "imap"))
	if err != nil {
		log.WithError(err).Warn("imap: ingest failed; leaving unread for retry")
		return
	}
	log.WithFields(logrus.Fields{
		"submission_id": res.SubmissionID,
		"state":         res.State,
		"duplicate":     res.IsDuplicate,
	}).Info("imap: message ingested")

	if err := mb.MarkSeen(ctx, m.UID); err != nil {
		log.WithError(err).Warn("imap: mark-seen failed")
	}
}
