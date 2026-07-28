package imap

import (
	"bytes"
	"context"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/delivery/emailingest"
	"github.com/atdayev/submission-triage/internal/model"
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
	Flag(ctx context.Context, uid uint32) error
	File(ctx context.Context, uid uint32, folder string) error
	// ScanFolder returns the Message-IDs in folder (read-only), capped at limit.
	ScanFolder(ctx context.Context, folder string, limit int) ([]string, error)
	Close() error
}

// ingester is what the poller needs from the service.
type ingester interface {
	IngestEmail(ctx context.Context, req service.IngestRequest) (service.IngestResult, error)
	AuditFileFailed(ctx context.Context, submissionID, folder, reason string)
	ReconcileHold(ctx context.Context, messageIDs []string) error
}

// statusFolders maps a submission's post-ingest status to its mailbox folder.
type statusFolders struct {
	complete, awaiting, escalated, unknownPolicy string
}

// Poller polls an IMAP inbox for new mail and ingests it.
type Poller struct {
	dial          func(ctx context.Context) (mailbox, error)
	ingest        ingester
	interval      time.Duration
	batchLimit    int
	mailbox       string
	fileByStatus  bool
	folders       statusFolders
	holdFolder    string
	holdScanLimit int
	log           *logrus.Entry

	lastPoll atomic.Int64 // unix nanos of the last successful fetch cycle
	failures atomic.Int64 // consecutive dial/fetch failures
}

// NewPoller returns a Poller configured from cfg, authenticating with auth.
func NewPoller(cfg config.IMAPConfig, auth Authenticator, svc ingester, log *logrus.Entry) *Poller {
	p := &Poller{
		dial:         dialIMAP(cfg, auth, log),
		ingest:       svc,
		interval:     cfg.PollInterval(),
		batchLimit:   defaultBatchLimit,
		mailbox:      cfg.Mailbox,
		fileByStatus: cfg.FileByStatus,
		folders: statusFolders{
			complete:      cfg.FolderComplete,
			awaiting:      cfg.FolderAwaiting,
			escalated:     cfg.FolderEscalated,
			unknownPolicy: cfg.FolderUnknownPolicy,
		},
		holdFolder:    cfg.FolderHold,
		holdScanLimit: cfg.HoldScanLimit,
		log:           log,
	}
	// seed with startup time so the first interval after boot isn't reported stale
	p.lastPoll.Store(time.Now().UnixNano())
	return p
}

// LastSuccessfulPoll reports when the poller last completed a fetch cycle.
func (p *Poller) LastSuccessfulPoll() time.Time { return time.Unix(0, p.lastPoll.Load()) }

// Configured reports that the poller is active; it exists only when IMAP is set.
func (p *Poller) Configured() bool { return true }

func (p *Poller) recordSuccess() {
	p.lastPoll.Store(time.Now().UnixNano())
	p.failures.Store(0)
}

func (p *Poller) recordFailure() int64 { return p.failures.Add(1) }

// Run polls until ctx is cancelled, fetching once at startup then per interval.
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
	if ctx.Err() != nil {
		return // shutting down: don't open a fresh connection
	}
	mb, err := p.dial(ctx)
	if err != nil {
		p.log.WithError(err).WithField("consecutive_failures", p.recordFailure()).Warn("imap connect failed; will retry next tick")
		return
	}
	defer mb.Close()

	msgs, err := mb.FetchUnseen(ctx, p.batchLimit)
	if err != nil {
		p.log.WithError(err).WithField("consecutive_failures", p.recordFailure()).Warn("imap fetch failed")
		return
	}
	// fetch cycle completed; a zero-message tick still proves the mailbox is reachable
	p.recordSuccess()
	if len(msgs) == 0 {
		p.log.Debug("imap poll: no unseen messages")
	} else {
		p.log.WithField("count", len(msgs)).Info("imap poll: processing unseen messages")
		for _, m := range msgs {
			if ctx.Err() != nil {
				return
			}
			p.process(ctx, mb, m)
		}
	}
	// after the INBOX pass (its UIDs are done) reconcile the Hold folder membership
	p.reconcileHold(ctx, mb)
}

// reconcileHold syncs on_hold to the Hold folder's current membership. Runs after
// the INBOX pass so selecting another folder can't invalidate in-flight UIDs.
func (p *Poller) reconcileHold(ctx context.Context, mb mailbox) {
	if p.holdFolder == "" || ctx.Err() != nil {
		return
	}
	ids, err := mb.ScanFolder(ctx, p.holdFolder, p.holdScanLimit)
	if err != nil {
		p.log.WithError(err).Warn("imap: hold folder scan failed")
		return
	}
	if err := p.ingest.ReconcileHold(ctx, ids); err != nil {
		p.log.WithError(err).Warn("hold reconcile failed")
	}
}

func (p *Poller) process(ctx context.Context, mb mailbox, m rawMessage) {
	rid := logger.GenerateRequestID()
	log := p.log.WithFields(logrus.Fields{logger.RequestIDField: rid, "uid": m.UID})
	ctx = logger.ContextWithRequestID(logger.ContextWithLogger(ctx, log), rid)

	// a panic on one message must not kill the poller
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("imap: panic processing message: %v", r)
		}
	}()

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

	// mark \Seen BEFORE filing: MOVE removes the source UID, so mark-seen must
	// come first (MOVE preserves flags, so the filed copy stays read). A mark-seen
	// failure leaves the message unread for a retry next tick (ingest is idempotent).
	if err := mb.MarkSeen(ctx, m.UID); err != nil {
		log.WithError(err).Warn("imap: mark-seen failed; leaving unread for retry")
		return
	}
	// flag before filing: MOVE invalidates the source UID, so flags go on the
	// original first; a flag failure is cosmetic (status lives in the DB + digest)
	if res.NeedsReview {
		if err := mb.Flag(ctx, m.UID); err != nil {
			log.WithError(err).Warn("imap: flag failed; message stays unflagged")
		}
	}
	folder := p.folderFor(res)
	if folder == "" || res.IsDuplicate {
		return
	}
	if err := mb.File(ctx, m.UID, folder); err != nil {
		// cosmetic: the message stays read in the inbox, unfiled. The real status is
		// in the DB and the daily digest, so this loses nothing correctness-wise.
		log.WithError(err).WithField("folder", folder).Warn("imap: file failed; message stays in inbox")
		p.ingest.AuditFileFailed(ctx, res.SubmissionID, folder, err.Error())
	}
}

// folderFor returns the status folder for the just-processed message, or "" to
// leave it in the inbox (filing disabled, or a non-terminal state).
func (p *Poller) folderFor(res service.IngestResult) string {
	if !p.fileByStatus {
		return ""
	}
	if res.PolicyUnknown {
		return p.folders.unknownPolicy
	}
	switch res.State {
	case model.StateComplete:
		return p.folders.complete
	case model.StateAwaiting:
		return p.folders.awaiting
	case model.StateEscalated:
		return p.folders.escalated
	default:
		return "" // open/closed: not filed
	}
}
