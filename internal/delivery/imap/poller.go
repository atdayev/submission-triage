// Package imap polls an existing mailbox over IMAP and feeds new mail into the
// same ingest pipeline the Postmark webhook uses. It is an additive inbound
// channel: enabled only when IMAP credentials are configured.
package imap

import (
	"bytes"
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/delivery/emailingest"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/postmarkeml"
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
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		p.process(ctx, mb, m)
	}
}

func (p *Poller) process(ctx context.Context, mb mailbox, m rawMessage) {
	payload, err := postmarkeml.FromReader(bytes.NewReader(m.Raw))
	if err != nil {
		// won't parse on a later tick either; mark read so we don't loop on it
		p.log.WithError(err).WithField("uid", m.UID).Warn("imap: unparseable message; marking read")
		_ = mb.MarkSeen(ctx, m.UID)
		return
	}

	if _, err := p.ingest.IngestEmail(ctx, emailingest.Translate(payload, "imap")); err != nil {
		// transient failure: leave unread for the next tick (reprocessing is idempotent)
		p.log.WithError(err).WithField("uid", m.UID).Warn("imap: ingest failed; leaving unread for retry")
		return
	}

	if err := mb.MarkSeen(ctx, m.UID); err != nil {
		p.log.WithError(err).WithField("uid", m.UID).Warn("imap: mark-seen failed")
	}
}
