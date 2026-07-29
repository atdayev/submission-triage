package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/email"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/pkg/logger"
	"github.com/atdayev/submission-triage/pkg/textutil"
)

// bodyPolicyScanBytes bounds how much of a reply body is searched for a line of
// business, so a long signature block can't outvote the sender's first sentence.
const bodyPolicyScanBytes = 512

// quoteMarkers start the quoted thread history in a reply body.
var quoteMarkers = []string{"\n>", "-----Original Message-----", "\nFrom:", "\n________________________________"}

var policyAliases = []struct {
	match  string
	policy string
}{
	{"general liability", "cgl"},
	{"cgl", "cgl"},
	{"business owners", "bop"},
	{"bop", "bop"},
	{"workers' comp", "workers_compensation"},
	{"workers comp", "workers_compensation"},
	{"property", "commercial_property"},
	{"cyber", "cyber_liability"},
}

// TextExtractor extracts text from a document's raw bytes.
type TextExtractor interface {
	Extract(data []byte) (string, error)
}

type clock func() time.Time

// Dependencies are the collaborators a SubmissionsService needs.
type Dependencies struct {
	Config         *config.Config
	Repository     *repository.Repository
	EmailSender    email.Sender
	Classifier     classifier.Classifier
	ChecklistStore checklist.Store
	TextExtractors map[string]TextExtractor
	LLM            llm.Client
	Log            *logrus.Entry
}

// SubmissionsService ingests emails, evaluates checklists, and dispatches replies.
type SubmissionsService struct {
	cfg        *config.Config
	repo       *repository.Repository
	mail       email.Sender
	classifier classifier.Classifier
	checklists checklist.Store
	extractors map[string]TextExtractor
	llm        llm.Client
	now        clock
	log        *logrus.Entry
	ingestSF   singleflight.Group

	replyJobs  chan replyJob
	replyWG    sync.WaitGroup // worker lifecycle
	inFlightWG sync.WaitGroup // per-enqueued-job

	replyCtx    context.Context // reply lifetime; canceled at Shutdown
	replyCancel context.CancelFunc
	drainGrace  time.Duration

	coalesceWindow    time.Duration // per-submission reply spacing
	outboxRetryAfter  time.Duration // only retry attempted entries older than this, so the online sender isn't double-dispatched
	outboxMaxAttempts int
	outboxBatch       int

	dispatchMu  sync.Mutex
	dispatching map[string]struct{} // outbox ids in flight; the sweeper skips them

	enqueueMu sync.RWMutex // serializes reply enqueue against Shutdown
	closed    bool         // set at Shutdown; guards the replyJobs channel

	cappedMu  sync.Mutex // guards cappedDay
	cappedDay string     // UTC day the llm.capped audit was last emitted

	replyDisabledOnce sync.Once // audits reply.disabled at most once per process
}

const (
	defaultReplyDrainGrace   = 10 * time.Second
	defaultOutboxRetryAfter  = 2 * time.Minute
	defaultOutboxMaxAttempts = 10
	defaultOutboxBatch       = 100
	markSentTimeout          = 2 * time.Second // detached mark-sent: bounds shutdown overrun
	auditFlushTimeout        = 5 * time.Second // detached audit flush: bounds shutdown overrun
	shutdownHardDeadline     = 5 * time.Second // past this a wait is abandoned so the process exits
	defaultDigestMaxRows     = 500             // fallback when DIGEST_MAX_ROWS is unset
	sweepBatch               = 100             // per-tick row cap on the escalation/closure sweeps
	missingAttachmentItemID  = "missing_attachment"
)

const (
	maxExtractedTextBytes = 256 << 10 // bound stored per-document extracted text
	pdfTrailerScanBytes   = 4 << 10   // tail of a PDF searched for the trailer dictionary
	pdfExtension          = ".pdf"
)

type replyJob struct {
	ctx          context.Context
	reply        model.Reply
	submissionID string
	outboxID     string
	version      time.Time // outbox row updated_at at enqueue; guards mark-sent against a supersede
}

// IngestRequest is a single inbound email to ingest.
type IngestRequest struct {
	MessageID   string
	InReplyTo   string
	References  []string
	FromAddress string
	FromName    string
	// ReplyTo is the address to reply to when set and parseable, else FromAddress.
	ReplyTo     string
	ToAddresses []string
	Subject     string
	BodyText    string
	ReceivedAt  time.Time
	Attachments []model.Attachment
	Source      string
	// AutoSubmitted marks mail from an automated agent (out-of-office, bulk,
	// list); such mail is stored but never replied to.
	AutoSubmitted bool
	// AutoResponseHeaders are the auto-response headers seen on the message,
	// recorded when a reply is suppressed.
	AutoResponseHeaders map[string]string
	// Bounce marks a delivery-status notification; a permanent bounce flags the
	// submission delivery-failed rather than being suppressed as generic auto mail.
	Bounce          bool
	BouncePermanent bool
}

// IngestResult is the outcome of ingesting one email.
type IngestResult struct {
	SubmissionID  string
	State         model.State
	IsDuplicate   bool
	PolicyUnknown bool
	NeedsReview   bool // a human should look; the poller flags the mail \Flagged
	MissingItems  []model.MissingItem
	ReplyQueued   bool
}

// AuditFileFailed records that filing a processed message into its status folder
// failed. Cosmetic (status lives in the DB + digest); the poller doesn't fail on it.
func (s *SubmissionsService) AuditFileFailed(ctx context.Context, submissionID, folder, reason string) {
	s.audit(ctx, submissionID, model.EventMailFileFailed, map[string]any{
		"folder": folder,
		"error":  reason,
	})
}

// auditCollector buffers an ingest's audit entries so one ingest writes its trail
// in a single pass at the end. The buffer is NOT part of the submission
// transaction: it is flushed on every exit path, including failures, because a
// dropped trail is unrecoverable while an orphan row is merely noise.
type auditCollector struct {
	entries []*model.AuditEntry
}

type auditCollectorKey struct{}

// NewSubmissionsService builds the service and starts its reply workers.
func NewSubmissionsService(d Dependencies) *SubmissionsService {
	workers := d.Config.Reply.Workers
	if workers <= 0 {
		workers = 4
	}
	queue := d.Config.Reply.QueueSize
	if queue <= 0 {
		queue = 64
	}
	s := &SubmissionsService{
		cfg:         d.Config,
		repo:        d.Repository,
		mail:        d.EmailSender,
		classifier:  d.Classifier,
		checklists:  d.ChecklistStore,
		extractors:  d.TextExtractors,
		llm:         d.LLM,
		now:         time.Now,
		log:         d.Log,
		replyJobs:   make(chan replyJob, queue),
		dispatching: map[string]struct{}{},
	}
	s.replyCtx, s.replyCancel = context.WithCancel(context.Background())
	s.drainGrace = d.Config.HTTP.ShutdownTimeout()
	if s.drainGrace <= 0 {
		s.drainGrace = defaultReplyDrainGrace
	}
	s.coalesceWindow = d.Config.Reply.CoalesceWindow()
	s.outboxRetryAfter = defaultOutboxRetryAfter
	s.outboxMaxAttempts = defaultOutboxMaxAttempts
	s.outboxBatch = defaultOutboxBatch
	for i := 0; i < workers; i++ {
		s.replyWG.Add(1)
		go s.replyWorker()
	}
	return s
}

// replyJobContext detaches a reply from the request lifecycle but keeps it
// cancelable at shutdown, carrying over request-scoped logging values.
func (s *SubmissionsService) replyJobContext(reqCtx context.Context) context.Context {
	jobCtx := logger.ContextWithLogger(s.replyCtx, logger.GetLoggerFromContext(reqCtx))
	if rid := logger.RequestIDFromContext(reqCtx); rid != "" {
		jobCtx = logger.ContextWithRequestID(jobCtx, rid)
	}
	return jobCtx
}

func (s *SubmissionsService) replyWorker() {
	defer s.replyWG.Done()
	for job := range s.replyJobs {
		s.dispatchOnline(job)
		s.inFlightWG.Done()
	}
}

// dispatchOnline is the low-latency path: send immediately, mark the outbox
// entry sent on success. On failure the entry stays pending for the sweeper.
func (s *SubmissionsService) dispatchOnline(job replyJob) {
	defer s.releaseDispatch(job.submissionID)
	if err := s.deliver(job.ctx, job.reply, job.submissionID); err != nil {
		logger.GetLoggerFromContext(job.ctx).WithError(err).Warn("reply send failed; will retry from outbox")
		s.audit(job.ctx, job.submissionID, model.EventReplyFailed, map[string]any{"error": err.Error()})
		return
	}
	// record the send even if the request/shutdown ctx is canceled, else it resends.
	// guard on the version so a follow-up that coalesced into this row isn't marked sent.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(job.ctx), markSentTimeout)
	defer cancel()
	if err := s.repo.Outbox.MarkSent(markCtx, job.outboxID, job.version); err != nil {
		s.log.WithError(err).WithField("outbox_id", job.outboxID).Warn("mark outbox sent failed")
	}
}

// claimDispatch takes exclusive ownership of a submission's outbound reply, reporting
// false if another sender already holds it. Keyed on the submission because that id is
// known *before* the outbox row commits, which is where it must be claimed — a
// committed-but-unclaimed row looks abandoned, and a concurrent sweep sends it too.
func (s *SubmissionsService) claimDispatch(submissionID string) bool {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	if s.dispatching == nil {
		s.dispatching = map[string]struct{}{}
	}
	if _, held := s.dispatching[submissionID]; held {
		return false
	}
	s.dispatching[submissionID] = struct{}{}
	return true
}

func (s *SubmissionsService) releaseDispatch(submissionID string) {
	s.dispatchMu.Lock()
	delete(s.dispatching, submissionID)
	s.dispatchMu.Unlock()
}

// enqueueReply hands a job to the worker pool. Returns false if the pool is
// shutting down or its queue is full, leaving the row for the outbox sweeper. The
// caller owns the dispatch claim and releases it when this returns false.
func (s *SubmissionsService) enqueueReply(job replyJob) bool {
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closed {
		return false
	}
	s.inFlightWG.Add(1)
	select {
	case s.replyJobs <- job:
		return true
	default:
		s.inFlightWG.Done()
		return false
	}
}

func (s *SubmissionsService) setClock(c clock) { s.now = c }

// IngestEmail ingests one email, deduping concurrent deliveries of the same message.
func (s *SubmissionsService) IngestEmail(ctx context.Context, req IngestRequest) (IngestResult, error) {
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = s.now()
	}

	emailID := computeEmailID(req.MessageID, req.BodyText, req.Attachments)

	// collapse parallel deliveries of the same email, else duplicate rows
	v, err, _ := s.ingestSF.Do(emailID, func() (any, error) {
		return s.ingestEmailInner(ctx, req, emailID)
	})
	if err != nil {
		return IngestResult{}, err
	}
	return v.(IngestResult), nil
}

func (s *SubmissionsService) ingestEmailInner(ctx context.Context, req IngestRequest, emailID string) (IngestResult, error) {
	collector := &auditCollector{}
	ctx = context.WithValue(ctx, auditCollectorKey{}, collector)
	defer s.flushAudit(ctx, collector)

	sub, dup, err := s.resolveSubmission(ctx, req, emailID)
	if err != nil {
		return IngestResult{}, err
	}
	if dup != nil {
		return *dup, nil
	}

	// a header match returns a submission with prior emails; a created one has none
	newSubmission := len(sub.Emails) == 0

	inbound := inboundEmail(req, emailID, sub.ID)
	sub.AttachEmail(inbound)
	// an automated sender's action (auto-reply or bounce) doesn't advance the escalation clock
	if !req.AutoSubmitted && !req.Bounce {
		sub.MarkAction(s.now())
	}
	// a fresh message from the broker clears a prior hard bounce and lets one reply
	// through, rather than staying mute forever on a since-fixed address. This proves
	// the address sends, not that it receives — a re-bounce sets the flag again.
	if !req.Bounce && sub.DeliveryFailed {
		sub.DeliveryFailed = false
		s.audit(ctx, sub.ID, model.EventDeliveryRetried, map[string]any{"from": req.FromAddress})
	}

	s.audit(ctx, sub.ID, model.EventEmailReceived, map[string]any{
		"deterministic_id": emailID,
		"message_id":       req.MessageID,
		"attachment_count": len(req.Attachments),
		"source":           req.Source,
	})

	// a delivery-status notification isn't a submission update: record it, flag the
	// unreachable address for a human, and never classify or reply to it
	if req.Bounce {
		return s.persistBounce(ctx, sub, req)
	}

	missing, policyUnknown := s.evaluate(ctx, sub, inbound)

	// pull the account identity once per submission (drives the digest lead and
	// bind-date escalation); only when a checklist matched, so an application can exist
	if !policyUnknown {
		s.extractIdentity(ctx, sub)
	}

	// content threading: a fresh email with no thread headers landed as a new
	// submission; if it matches an existing open one by named insured + sender,
	// attach to that instead of duplicating
	if newSubmission {
		if merged, ok := s.contentMatch(ctx, sub, inbound, collector); ok {
			sub = merged
			missing = sub.MissingItems
			policyUnknown = sub.PolicyType == model.PolicyTypeUnknown
		}
	}

	// the review flag and the broker reply are decided independently: an unreadable
	// file doesn't suppress the request for the documents that are genuinely absent
	prevReview := sub.NeedsReview
	sub.NeedsReview = model.RequiresReview(missing) || s.policyClarifyExhausted(sub)
	if sub.NeedsReview && !prevReview {
		s.audit(ctx, sub.ID, model.EventNeedsReview, map[string]any{
			"documents": s.reviewDocs(sub),
		})
	}

	if err := s.transitionState(ctx, sub, missing); err != nil {
		return IngestResult{}, err
	}

	result := IngestResult{
		SubmissionID:  sub.ID,
		State:         sub.State,
		NeedsReview:   sub.NeedsReview,
		MissingItems:  missing,
		PolicyUnknown: policyUnknown,
	}

	if req.AutoSubmitted {
		return s.persistSuppressed(ctx, sub, req.AutoResponseHeaders, result)
	}
	reply, fingerprint, sendReply := s.buildReply(sub, missing, inbound, policyUnknown)
	switch {
	case !sendReply:
		// nothing the broker can act on (flagged-only, or the clarification cap is spent)
		return s.persistNoReply(ctx, sub, result)
	case s.replySuppressed(ctx, sub):
		// a hard bounce, a human hold, or the kill switch
		return s.persistNoReply(ctx, sub, result)
	case fingerprint == sub.LastReplyHash:
		// same ask as last time: a content-free "thanks, will send tomorrow" must not
		// draw an identical list, and a complete submission must not be thanked twice
		s.audit(ctx, sub.ID, model.EventReplySuppressed, map[string]any{"reason": "unchanged"})
		return s.persistNoReply(ctx, sub, result)
	}
	sub.LastReplyHash = fingerprint
	// submission and reply commit in one tx: an inbound email without an outbox
	// row is unrecoverable (dedup blocks the retry). On failure the poller retries.
	now := s.now()
	notBefore := s.replyNotBefore(sub, now)
	entry := &model.OutboxEntry{ID: uuid.NewString(), SubmissionID: sub.ID, Reply: reply, Status: model.OutboxPending, NotBefore: notBefore}
	// claim before the row exists: the instant it commits it is visible to the outbox
	// sweeper, and an unclaimed pending row is one the sweeper will also send
	claimed := s.claimDispatch(sub.ID)
	if err := s.repo.Submissions.UpsertSubmissionWithReply(ctx, sub, entry); err != nil {
		if claimed {
			s.releaseDispatch(sub.ID)
		}
		return IngestResult{}, fmt.Errorf("persist submission with reply: %w", err)
	}
	result.ReplyQueued = true

	// dispatch online only when due now (the first reply, or once the coalesce
	// window has elapsed); otherwise the flush sweeper sends it after not_before
	if claimed && !notBefore.After(now) {
		job := replyJob{ctx: s.replyJobContext(ctx), reply: reply, submissionID: sub.ID, outboxID: entry.ID, version: entry.UpdatedAt}
		if s.enqueueReply(job) {
			return result, nil // the worker releases the claim
		}
	}
	if claimed {
		s.releaseDispatch(sub.ID)
	}
	return result, nil
}

// replyNotBefore enforces per-submission spacing: the first reply (no prior)
// goes now; a follow-up waits until coalesceWindow after the previous send.
func (s *SubmissionsService) replyNotBefore(sub *model.Submission, now time.Time) time.Time {
	if sub.LastReplyAt.IsZero() {
		return now
	}
	if scheduled := sub.LastReplyAt.Add(s.coalesceWindow); scheduled.After(now) {
		return scheduled
	}
	return now
}

// persistSuppressed commits an auto-submitted email without queuing a reply.
// The single-tx invariant is relaxed here on purpose: with no reply to recover,
// committing the email alone is safe.
func (s *SubmissionsService) persistSuppressed(ctx context.Context, sub *model.Submission, headers map[string]string, result IngestResult) (IngestResult, error) {
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("persist suppressed submission: %w", err)
	}
	s.audit(ctx, sub.ID, model.EventReplySuppressed, map[string]any{
		"reason":  "auto_submitted",
		"headers": headers,
	})
	return result, nil
}

// persistBounce records a delivery-status notification. A permanent bounce flags the
// submission delivery_failed and for review; a transient one is only audited. No reply
// is ever queued — a bounce is machine mail.
func (s *SubmissionsService) persistBounce(ctx context.Context, sub *model.Submission, req IngestRequest) (IngestResult, error) {
	if req.BouncePermanent {
		sub.DeliveryFailed = true
		sub.NeedsReview = true
	}
	s.audit(ctx, sub.ID, model.EventReplyBounced, map[string]any{
		"from":      req.FromAddress,
		"permanent": req.BouncePermanent,
	})
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("persist bounce: %w", err)
	}
	return IngestResult{SubmissionID: sub.ID, State: sub.State, NeedsReview: sub.NeedsReview}, nil
}

// replySuppressed reports whether outbound replies are currently blocked for this
// submission — a hard-bounced address, a human hold, or the global kill switch —
// and audits why. The kill switch audits per submission: nothing is ever resent
// automatically, so this trail is the only record of what was skipped.
func (s *SubmissionsService) replySuppressed(ctx context.Context, sub *model.Submission) bool {
	switch {
	case s.cfg.Reply.RepliesEnabled != nil && !*s.cfg.Reply.RepliesEnabled:
		s.warnRepliesDisabledOnce()
		s.audit(ctx, sub.ID, model.EventReplyDisabled, map[string]any{"reason": "kill_switch"})
		return true
	case sub.DeliveryFailed:
		s.audit(ctx, sub.ID, model.EventReplySuppressed, map[string]any{"reason": "delivery_failed"})
		return true
	case sub.OnHold:
		s.audit(ctx, sub.ID, model.EventReplySuppressed, map[string]any{"reason": "on_hold"})
		return true
	}
	return false
}

// persistNoReply commits a submission with no broker reply: everything outstanding
// is flagged agency-side, so there is nothing to ask and no reply to recover.
func (s *SubmissionsService) persistNoReply(ctx context.Context, sub *model.Submission, result IngestResult) (IngestResult, error) {
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("persist submission: %w", err)
	}
	return result, nil
}

// reviewDocs lists the classified documents that need a human — unreadable, or
// below the confidence floor.
func (s *SubmissionsService) reviewDocs(sub *model.Submission) []map[string]any {
	floor := s.cfg.Classifier.ConfidenceFloor
	var out []map[string]any
	for i := range sub.Documents {
		d := &sub.Documents[i]
		if d.ClassifiedAs == "" || (!d.Unreadable && d.Confidence >= floor) {
			continue
		}
		out = append(out, map[string]any{
			"document_id":   d.ID,
			"filename":      d.Filename,
			"classified_as": d.ClassifiedAs,
			"confidence":    d.Confidence,
			"unreadable":    d.Unreadable,
		})
	}
	return out
}

// resolveSubmission finds the submission this email belongs to, or creates one.
// dup is non-nil when the email was already ingested; the caller returns it as-is.
func (s *SubmissionsService) resolveSubmission(ctx context.Context, req IngestRequest, emailID string) (sub *model.Submission, dup *IngestResult, err error) {
	refs := cleanThreadRefs(req.MessageID, req.InReplyTo, req.References)
	found, ambiguous, ferr := s.repo.Submissions.FindByEmailReference(ctx, refs)
	switch {
	case ferr == nil:
		for _, e := range found.Emails {
			if e.DeterministicID == emailID {
				s.audit(ctx, found.ID, model.EventEmailDuplicate, map[string]any{
					"deterministic_id": emailID,
					"source":           req.Source,
				})
				return nil, &IngestResult{SubmissionID: found.ID, State: found.State, IsDuplicate: true}, nil
			}
		}
		if ambiguous {
			s.audit(ctx, found.ID, model.EventThreadAmbiguous, map[string]any{"refs": refs})
		}
		s.audit(ctx, found.ID, model.EventThreadMatched, map[string]any{
			"matched_by": "thread_headers",
			"message_id": req.MessageID,
		})
		return found, nil, nil
	case errors.Is(ferr, model.ErrSubmissionNotFound):
		// dedup a redelivered email on its deterministic id before creating a new one
		if existing, dErr := s.repo.Submissions.FindByDeterministicID(ctx, emailID); dErr == nil {
			s.audit(ctx, existing.ID, model.EventEmailDuplicate, map[string]any{
				"deterministic_id": emailID,
				"source":           req.Source,
			})
			return nil, &IngestResult{SubmissionID: existing.ID, State: existing.State, IsDuplicate: true}, nil
		} else if !errors.Is(dErr, model.ErrSubmissionNotFound) {
			return nil, nil, fmt.Errorf("dedup by deterministic id: %w", dErr)
		}
		created, cerr := s.createSubmission(ctx, req)
		if cerr != nil {
			return nil, nil, fmt.Errorf("create submission: %w", cerr)
		}
		return created, nil, nil
	default:
		return nil, nil, fmt.Errorf("find by reference: %w", ferr)
	}
}

// contentMatch attaches a freshly-created submission to an existing open one with the
// same named insured and sender inside the match window, re-parenting its buffered
// audits and re-evaluating over the combined documents. The conditions are strict on
// purpose: a looser match would merge a renewal with new business for the same insured.
func (s *SubmissionsService) contentMatch(ctx context.Context, newSub *model.Submission, inbound model.Email, collector *auditCollector) (*model.Submission, bool) {
	if newSub.NamedInsured == "" {
		return nil, false
	}
	cutoff := s.now().Add(-s.cfg.Threading.MatchWindow()).UnixNano()
	existing, err := s.repo.Submissions.FindContentMatch(ctx, newSub.NamedInsured, newSub.FromAddress, cutoff)
	if err != nil {
		if !errors.Is(err, model.ErrSubmissionNotFound) {
			s.log.WithError(err).Warn("content match lookup failed")
		}
		return nil, false
	}
	reparentAudits(collector, newSub.ID, existing.ID)

	inbound.SubmissionID = existing.ID
	existing.AttachEmail(inbound)
	for _, d := range newSub.Documents {
		d.SubmissionID = existing.ID
		existing.Documents = append(existing.Documents, d)
	}
	if existing.NamedInsured == "" {
		existing.NamedInsured = newSub.NamedInsured
		existing.EffectiveDate = newSub.EffectiveDate
	}
	existing.MarkAction(s.now())
	if cl, ok := s.checklists.Get(existing.PolicyType); ok {
		existing.MissingItems = model.EvaluateChecklist(*existing, cl, model.ChecklistOptions{
			ConfidenceFloor: s.cfg.Classifier.ConfidenceFloor,
		})
	}
	s.audit(ctx, existing.ID, model.EventThreadMatchedByContent, map[string]any{
		"named_insured": existing.NamedInsured,
		"from":          existing.FromAddress,
		"email_id":      inbound.DeterministicID,
	})
	return existing, true
}

// reparentAudits rewrites buffered audit entries from a phantom (never-persisted)
// submission id to the one the email was ultimately merged into.
func reparentAudits(c *auditCollector, from, to string) {
	for _, e := range c.entries {
		if e.SubmissionID == from {
			e.SubmissionID = to
		}
	}
}

// inboundEmail maps an ingest request to the stored inbound email record.
func inboundEmail(req IngestRequest, emailID, submissionID string) model.Email {
	return model.Email{
		DeterministicID: emailID,
		SubmissionID:    submissionID,
		Direction:       model.DirectionInbound,
		MessageID:       req.MessageID,
		InReplyTo:       req.InReplyTo,
		References:      req.References,
		FromAddress:     req.FromAddress,
		FromName:        req.FromName,
		ReplyTo:         req.ReplyTo,
		ToAddresses:     req.ToAddresses,
		Subject:         req.Subject,
		BodyText:        req.BodyText,
		ReceivedAt:      req.ReceivedAt,
		Attachments:     req.Attachments,
	}
}

// resolvePolicyType re-infers an undetermined policy type from this email. It runs
// on every inbound, not only the first: real subjects are often "New Business
// Submission" or a bare account name, and the answer arrives in a later reply.
func (s *SubmissionsService) resolvePolicyType(ctx context.Context, sub *model.Submission, inbound model.Email) {
	if _, ok := s.checklists.Get(sub.PolicyType); ok {
		return
	}
	all := s.checklists.All()
	resolved := inferPolicyType(inbound.Subject, all)
	if resolved == model.PolicyTypeUnknown {
		resolved = inferPolicyType(replyBodyPrefix(inbound.BodyText), all)
	}
	if resolved == model.PolicyTypeUnknown {
		return
	}
	sub.PolicyType = resolved
	s.audit(ctx, sub.ID, model.EventPolicyResolved, map[string]any{
		"policy_type": resolved,
		"asks":        sub.PolicyClarifyAsks,
	})
}

// replyBodyPrefix returns the sender's own text: everything above the quoted thread,
// capped. Below the quote line sits our own clarification listing every line of
// business we support, which would otherwise match whatever appears there first.
func replyBodyPrefix(body string) string {
	for _, marker := range quoteMarkers {
		if i := strings.Index(body, marker); i >= 0 {
			body = body[:i]
		}
	}
	if len(body) > bodyPolicyScanBytes {
		body = body[:bodyPolicyScanBytes]
	}
	return body
}

// policyClarifyExhausted reports that the clarification has been asked as often as
// the cap allows; past it the ask stops and the submission is flagged for a human.
func (s *SubmissionsService) policyClarifyExhausted(sub *model.Submission) bool {
	maxAsks := s.cfg.Reply.PolicyClarifyMaxAsks
	return maxAsks > 0 && sub.PolicyClarifyAsks > maxAsks
}

// evaluate classifies the inbound attachments, runs the checklist, and records
// the result. policyUnknown means no checklist matched the policy type.
func (s *SubmissionsService) evaluate(ctx context.Context, sub *model.Submission, inbound model.Email) (missing []model.MissingItem, policyUnknown bool) {
	s.resolvePolicyType(ctx, sub, inbound)
	checklistDef, hasChecklist := s.checklists.Get(sub.PolicyType)
	policyUnknown = !hasChecklist
	if policyUnknown {
		sub.PolicyType = model.PolicyTypeUnknown
		sub.PolicyClarifyAsks++
		s.audit(ctx, sub.ID, model.EventPolicyUnknown, map[string]any{
			"subject_hint": inbound.Subject,
			"asks":         sub.PolicyClarifyAsks,
		})
		missing = []model.MissingItem{model.PolicyUnknownItem()}
	} else {
		for _, d := range s.classifyAttachments(ctx, sub, inbound, checklistDef) {
			sub.AttachDocument(d)
		}
		missing = model.EvaluateChecklist(*sub, checklistDef, model.ChecklistOptions{
			ConfidenceFloor: s.cfg.Classifier.ConfidenceFloor,
		})
	}
	sub.MissingItems = missing
	missingIDs := make([]string, 0, len(missing))
	for _, m := range missing {
		missingIDs = append(missingIDs, m.ID)
	}
	s.audit(ctx, sub.ID, model.EventChecklistEvaluated, map[string]any{
		"policy_type":   sub.PolicyType,
		"missing_count": len(missing),
		"missing_ids":   missingIDs,
	})
	return missing, policyUnknown
}

// transitionState moves the submission to complete (nothing outstanding) or
// awaiting and audits the change; the review flag is set separately.
func (s *SubmissionsService) transitionState(ctx context.Context, sub *model.Submission, missing []model.MissingItem) error {
	prev := sub.State
	next := model.StateComplete
	if len(missing) > 0 {
		next = model.StateAwaiting
	}
	if err := sub.TransitionTo(next, s.now()); err != nil {
		return fmt.Errorf("transition: %w", err)
	}
	if prev != sub.State {
		s.audit(ctx, sub.ID, model.EventStateTransitioned, map[string]any{
			"from": string(prev),
			"to":   string(sub.State),
		})
	}
	return nil
}

// Wait blocks until queued replies finish. Re-entrant; doesn't stop the pool.
func (s *SubmissionsService) Wait() { s.inFlightWG.Wait() }

// Shutdown drains queued replies, then stops the workers. Call once. Past the
// grace window, in-flight sends are canceled so the process still exits.
func (s *SubmissionsService) Shutdown() {
	s.enqueueMu.Lock()
	if s.closed {
		s.enqueueMu.Unlock()
		return
	}
	s.closed = true
	s.enqueueMu.Unlock()

	defer s.replyCancel()
	if !waitBounded(&s.inFlightWG, s.drainGrace) {
		s.log.Warn("reply drain exceeded grace; canceling in-flight sends")
		s.replyCancel()
		// a Sender that ignores cancellation would otherwise hang shutdown forever
		if !waitBounded(&s.inFlightWG, shutdownHardDeadline) {
			s.log.Error("in-flight replies ignored cancellation; abandoning them so the process can exit")
		}
	}
	close(s.replyJobs)
	if !waitBounded(&s.replyWG, shutdownHardDeadline) {
		s.log.Error("reply workers did not stop; abandoning them so the process can exit")
	}
}

// waitBounded waits for wg, reporting false if d elapsed first.
func waitBounded(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *SubmissionsService) knownChecklistNames() []string {
	all := s.checklists.All()
	out := make([]string, 0, len(all))
	for _, c := range all {
		if c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}

// buildReply decides the outbound reply, if any: policy-unknown clarification,
// completion note, missing-items request, or none when only flagged items remain.
// The fingerprint covers the items the reply actually names, so the caller can tell
// a genuinely new ask from a repeat of the last one.
func (s *SubmissionsService) buildReply(sub *model.Submission, missing []model.MissingItem, inbound model.Email, policyUnknown bool) (reply model.Reply, fingerprint string, send bool) {
	switch {
	case policyUnknown:
		if s.policyClarifyExhausted(sub) {
			return model.Reply{}, "", false
		}
		return model.BuildPolicyUnknownReply(*sub, inbound, s.knownChecklistNames()),
			model.ReplyFingerprint(model.ReplyKindPolicyUnknown, missing), true
	case len(missing) == 0:
		return model.BuildCompletionReply(*sub, inbound),
			model.ReplyFingerprint(model.ReplyKindCompletion, nil), true
	default:
		broker := model.BrokerActionable(missing)
		if len(broker) == 0 {
			return model.Reply{}, "", false
		}
		// the broker referenced files but attached none (often a share link): ask
		// them to attach directly rather than listing every document as absent
		if s.looksLikeMissingAttachment(inbound, broker) {
			broker = []model.MissingItem{{ID: missingAttachmentItemID, Code: model.ReasonMissingAttachment}}
		}
		return model.BuildMissingItemsReply(*sub, broker, inbound),
			model.ReplyFingerprint(model.ReplyKindMissingItems, broker), true
	}
}

// looksLikeMissingAttachment reports the A4 signal: this email carried no
// attachments, documents are still outstanding, and the body either names a
// file-sharing host or mentions attaching.
func (s *SubmissionsService) looksLikeMissingAttachment(inbound model.Email, broker []model.MissingItem) bool {
	if len(inbound.Attachments) > 0 || len(broker) == 0 {
		return false
	}
	body := strings.ToLower(inbound.BodyText)
	for _, d := range s.cfg.Document.AttachmentLinkDomains {
		if d != "" && strings.Contains(body, strings.ToLower(d)) {
			return true
		}
	}
	return strings.Contains(body, "attach")
}

// deliver sends a reply and, on success, records the outbound email and audits
// reply.sent. It does not touch the outbox; callers own that state transition.
func (s *SubmissionsService) deliver(ctx context.Context, reply model.Reply, submissionID string) error {
	providerMsgID, err := s.mail.SendThreadedReply(ctx, reply)
	if err != nil {
		return err
	}
	outbound := outboundEmail(reply, providerMsgID, s.now())
	if err := s.repo.Submissions.UpsertEmail(ctx, &outbound); err != nil {
		s.log.WithError(err).WithField("submission_id", submissionID).Warn("outbound email upsert failed")
	}
	// record the send time so the next reply on this submission is spaced by the
	// window; detached from cancellation like mark-sent so a send during shutdown
	// still records (else the next reply skips the window once after restart)
	lrCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markSentTimeout)
	defer cancel()
	if err := s.repo.Submissions.SetLastReplyAt(lrCtx, submissionID, s.now()); err != nil {
		s.log.WithError(err).WithField("submission_id", submissionID).Warn("set last_reply_at failed")
	}
	s.audit(ctx, submissionID, model.EventReplySent, map[string]any{
		"provider_msg_id": providerMsgID,
		"to":              reply.ToAddress,
		"subject":         reply.Subject,
		"via":             s.mail.Name(),
	})
	return nil
}

// RedeliverOutbox resends pending replies the online path didn't get out, dead-lettering
// after outboxMaxAttempts. Suppression is re-checked here, not only at build time: a
// submission can be held or hard-bounce after its reply was queued.
func (s *SubmissionsService) RedeliverOutbox(ctx context.Context) error {
	if s.cfg.Reply.RepliesEnabled != nil && !*s.cfg.Reply.RepliesEnabled {
		s.warnRepliesDisabledOnce()
		return nil
	}
	now := s.now()
	cutoff := now.Add(-s.outboxRetryAfter)
	rows, err := s.repo.Outbox.ListPending(ctx, now, cutoff, s.outboxBatch)
	if err != nil {
		return fmt.Errorf("outbox: list pending: %w", err)
	}
	blocked, err := s.blockedSubmissions(ctx, rows)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if blocked[row.SubmissionID] {
			continue // held or hard-bounced since this reply was queued
		}
		// exclusive per submission: skips a row the online path owns, and stops two
		// concurrent sweeps from both delivering the same reply
		if !s.claimDispatch(row.SubmissionID) {
			continue
		}
		s.redeliverOne(ctx, row)
	}
	return nil
}

// redeliverOne sends one due row and settles it. The caller holds the dispatch
// claim; this releases it.
func (s *SubmissionsService) redeliverOne(ctx context.Context, listed model.OutboxEntry) {
	defer s.releaseDispatch(listed.SubmissionID)
	// re-read under the claim: ListPending's result is a snapshot, and by now the
	// online path may have sent this row (send it again and the broker gets two) or a
	// follow-up may have superseded its text (send the snapshot and it's stale)
	fresh, ok, err := s.repo.Outbox.GetPending(ctx, listed.ID)
	if err != nil {
		s.log.WithError(err).WithField("outbox_id", listed.ID).Warn("outbox: re-read failed; skipping this tick")
		return
	}
	if !ok {
		return // already sent or dead-lettered since the list
	}
	row := *fresh
	if err := s.deliver(ctx, row.Reply, row.SubmissionID); err != nil {
		attempts := row.Attempts + 1
		if attempts < s.outboxMaxAttempts {
			_ = s.repo.Outbox.Update(ctx, row.ID, model.OutboxPending, attempts, err.Error())
			return
		}
		_ = s.repo.Outbox.Update(ctx, row.ID, model.OutboxFailed, attempts, err.Error())
		// the broker never received this ask, so let it be made again rather than
		// having the repeat-suppression gate strand the submission
		if cerr := s.repo.Submissions.ClearReplyHash(ctx, row.SubmissionID); cerr != nil {
			s.log.WithError(cerr).WithField("submission_id", row.SubmissionID).Warn("clear reply hash failed")
		}
		s.audit(ctx, row.SubmissionID, model.EventReplyFailed, map[string]any{
			"error": err.Error(), "attempts": attempts, "dead_lettered": true,
		})
		return
	}
	// guard on the version read from ListPending: a follow-up that coalesced into
	// this row since then stays pending for the next sweep, not lost
	_ = s.repo.Outbox.MarkSent(ctx, row.ID, row.UpdatedAt)
}

// blockedSubmissions returns which of these rows' submissions must not be sent to.
func (s *SubmissionsService) blockedSubmissions(ctx context.Context, rows []model.OutboxEntry) (map[string]bool, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.SubmissionID]; ok {
			continue
		}
		seen[row.SubmissionID] = struct{}{}
		ids = append(ids, row.SubmissionID)
	}
	found, err := s.repo.Submissions.ReplyBlockedIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("outbox: reply-blocked lookup: %w", err)
	}
	blocked := make(map[string]bool, len(found))
	for _, id := range found {
		blocked[id] = true
	}
	return blocked, nil
}

// CheckEscalations escalates on two triggers: idle past the policy's quiet threshold,
// and — regardless of quiet time — a near effective date, rate-limited to one nudge per
// MinGap. Held and delivery-failed submissions are excluded by the queries.
func (s *SubmissionsService) CheckEscalations(ctx context.Context) error {
	now := s.now()
	globalThreshold := s.cfg.Escalation.Threshold()
	minThreshold := globalThreshold
	for _, c := range s.checklists.All() {
		t := time.Duration(c.Escalation.ThresholdHours) * time.Hour
		if t > 0 && t < minThreshold {
			minThreshold = t
		}
	}
	stale, err := s.repo.Submissions.ListStale(ctx, now.Add(-minThreshold).UnixNano(), 100)
	if err != nil {
		return fmt.Errorf("list stale: %w", err)
	}
	escalated := map[string]bool{}
	for i := range stale {
		sub := &stale[i]
		threshold := globalThreshold
		if cl, ok := s.checklists.Get(sub.PolicyType); ok && cl.Escalation.ThresholdHours > 0 {
			threshold = time.Duration(cl.Escalation.ThresholdHours) * time.Hour
		}
		if now.Sub(sub.LastActionAt) < threshold {
			continue
		}
		if s.escalate(ctx, sub, now, "quiet_threshold") {
			escalated[sub.ID] = true
		}
	}

	bindWindow := s.cfg.Escalation.BindWindow()
	if bindWindow <= 0 {
		return nil
	}
	minGap := s.cfg.Escalation.MinGap()
	// floor the grace bound to midnight UTC: effective dates are stored at midnight,
	// so a tighter bound would drop a submission partway through its final day
	graceFloor := startOfUTCDay(now.Add(-s.cfg.Escalation.BindGrace()))
	soon, err := s.repo.Submissions.ListBindSoon(ctx, graceFloor.UnixNano(), now.Add(bindWindow).UnixNano(), sweepBatch)
	if err != nil {
		return fmt.Errorf("list bind-soon: %w", err)
	}
	for i := range soon {
		sub := &soon[i]
		if escalated[sub.ID] {
			continue // already handled by the quiet-time pass this tick
		}
		if sub.LastEscalatedAt != nil && now.Sub(*sub.LastEscalatedAt) < minGap {
			continue // nudged too recently
		}
		s.escalate(ctx, sub, now, "bind_window")
	}
	return nil
}

// escalate moves an awaiting submission to escalated when the broker can act, stamping
// LastEscalatedAt to rate-limit bind-window re-nudges. One blocked solely on an
// unreadable or low-confidence file is the agency's, and never escalates.
func (s *SubmissionsService) escalate(ctx context.Context, sub *model.Submission, now time.Time, reason string) bool {
	if len(model.BrokerActionable(sub.MissingItems)) == 0 {
		return false
	}
	if err := sub.TransitionTo(model.StateEscalated, now); err != nil {
		s.log.WithError(err).WithField("submission_id", sub.ID).Warn("could not escalate")
		return false
	}
	t := now
	sub.LastEscalatedAt = &t
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		s.log.WithError(err).WithField("submission_id", sub.ID).Warn("escalation upsert failed")
		return false
	}
	s.audit(ctx, sub.ID, model.EventEscalated, map[string]any{
		"reason":         reason,
		"last_action_at": sub.LastActionAt,
	})
	return true
}

// CheckClosures auto-closes submissions gone quiet: completed, escalated, and —
// on a longer window — awaiting or open ones no other sweep reaches.
func (s *SubmissionsService) CheckClosures(ctx context.Context) error {
	now := s.now()
	if autoClose := s.cfg.Escalation.AutoCloseAfter(); autoClose > 0 {
		if err := s.closeSettled(ctx, now, now.Add(-autoClose)); err != nil {
			return err
		}
	}
	// an awaiting submission whose only gap is agency-side can never escalate, and a
	// bounce-created open one is selected by no other query at all; without this
	// sweep both sit in every digest forever
	stalledAfter := s.cfg.Escalation.AwaitingAutoCloseAfter()
	if stalledAfter <= 0 {
		return nil
	}
	stalled, err := s.repo.Submissions.ListStalledBefore(ctx, now.Add(-stalledAfter).UnixNano(), sweepBatch)
	if err != nil {
		return fmt.Errorf("list stalled-before: %w", err)
	}
	for i := range stalled {
		s.closeQuiet(ctx, &stalled[i], "auto_close_stalled", now)
	}
	return nil
}

// closeSettled retires completed and escalated submissions quiet past the cutoff.
func (s *SubmissionsService) closeSettled(ctx context.Context, now, cutoff time.Time) error {
	subs, err := s.repo.Submissions.ListCompletedBefore(ctx, cutoff.UnixNano(), sweepBatch)
	if err != nil {
		return fmt.Errorf("list completed-before: %w", err)
	}
	for i := range subs {
		s.closeQuiet(ctx, &subs[i], "auto_close_after_complete", now)
	}

	esc, err := s.repo.Submissions.ListEscalatedSince(ctx, 0, 500)
	if err != nil {
		return fmt.Errorf("list escalated: %w", err)
	}
	for i := range esc {
		if esc[i].LastActionAt.After(cutoff) {
			continue
		}
		s.closeQuiet(ctx, &esc[i], "auto_close_after_escalation", now)
	}
	return nil
}

func (s *SubmissionsService) closeQuiet(ctx context.Context, sub *model.Submission, reason string, now time.Time) {
	if err := sub.TransitionTo(model.StateClosed, now); err != nil {
		s.log.WithError(err).WithField("submission_id", sub.ID).Warn("could not auto-close")
		return
	}
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		s.log.WithError(err).WithField("submission_id", sub.ID).Warn("auto-close upsert failed")
		return
	}
	s.audit(ctx, sub.ID, model.EventClosed, map[string]any{
		"reason":        reason,
		"auto_close_at": now,
	})
}

// ReconcileHold syncs on_hold to the Hold folder's membership. Folder membership is the
// state — declarative and idempotent, so running it every tick is drift-free.
func (s *SubmissionsService) ReconcileHold(ctx context.Context, messageIDs []string) error {
	inFolder, err := s.repo.Submissions.SubmissionIDsByMessageIDs(ctx, messageIDs)
	if err != nil {
		return fmt.Errorf("resolve hold folder: %w", err)
	}
	held, err := s.repo.Submissions.ListHeldIDs(ctx)
	if err != nil {
		return fmt.Errorf("list held: %w", err)
	}
	inSet := toSet(inFolder)
	heldSet := toSet(held)

	var toHold, toRelease []string
	for _, id := range inFolder {
		if _, ok := heldSet[id]; !ok {
			toHold = append(toHold, id)
		}
	}
	for _, id := range held {
		if _, ok := inSet[id]; !ok {
			toRelease = append(toRelease, id)
		}
	}
	if err := s.repo.Submissions.SetOnHold(ctx, toHold, true); err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	if err := s.repo.Submissions.SetOnHold(ctx, toRelease, false); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	// a reply queued before the hold describes pre-hold state; a human intervened, so
	// drop it rather than sending stale text the moment the hold lifts. The next
	// inbound email or escalation queues a reply that reflects where the case is now.
	if n, err := s.repo.Outbox.ExpirePendingForSubmissions(ctx, toRelease, "stale: queued before hold"); err != nil {
		s.log.WithError(err).Warn("expiring pre-hold replies failed")
	} else if n > 0 {
		s.log.WithField("count", n).Info("dropped replies queued before a hold")
	}
	for _, id := range toHold {
		s.audit(ctx, id, model.EventHeld, nil)
	}
	for _, id := range toRelease {
		s.audit(ctx, id, model.EventReleased, nil)
	}
	if len(toHold) > 0 || len(toRelease) > 0 {
		s.log.WithField("held", len(toHold)).WithField("released", len(toRelease)).Info("hold folder reconciled")
	}
	return nil
}

func toSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// SendDigest emails the agency a daily status digest of every open submission.
// No-ops without a recipient/interval, or when nothing is open (an empty digest
// trains the agency to ignore the address).
func (s *SubmissionsService) SendDigest(ctx context.Context) error {
	interval := s.cfg.Digest.Interval()
	if interval <= 0 {
		return nil
	}
	recipient := s.firstDigestRecipient()
	if recipient == "" {
		// escalation sends no mail of its own, so the digest is the only status *email*
		// there is; mailbox filing still runs, but nothing reaches a human who isn't looking
		s.log.Error("DIGEST_RECIPIENT is not set; no status email will be sent to anyone (mailbox filing still runs)")
		return nil
	}
	maxRows := s.cfg.Digest.MaxRows
	if maxRows <= 0 {
		maxRows = defaultDigestMaxRows
	}
	subs, err := s.repo.Submissions.ListOpen(ctx, maxRows)
	if err != nil {
		return fmt.Errorf("list open: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	omitted := 0
	if len(subs) == maxRows { // cap possibly hit; get the exact overflow
		if total, cerr := s.repo.Submissions.CountOpen(ctx); cerr == nil && total > maxRows {
			omitted = total - maxRows
		}
	}
	ids := make([]string, len(subs))
	for i := range subs {
		ids[i] = subs[i].ID
	}
	flaggedIDs, err := s.repo.Submissions.FilenameOnlyItems(ctx, ids)
	if err != nil {
		return fmt.Errorf("filename-only items: %w", err)
	}
	reply := model.Reply{
		ToAddress: recipient,
		Subject:   fmt.Sprintf("[submission-triage] Daily digest (%d open)", len(subs)+omitted),
		BodyText: model.BuildDigest(subs, s.resolveFilenameOnly(subs, flaggedIDs),
			s.replyWithheld(ctx, subs), omitted, s.now()),
	}
	if _, err := s.mail.SendThreadedReply(ctx, reply); err != nil {
		return fmt.Errorf("send digest: %w", err)
	}
	s.audit(ctx, "", model.EventDigestSent, map[string]any{
		"recipient": recipient,
		"open":      len(subs),
		"omitted":   omitted,
	})
	return nil
}

// replyWithheld returns the digest rows whose reply the kill switch swallowed.
// Nothing is ever resent automatically when the switch goes back on, so a
// submission that never got any reply is work the agency has to pick up by hand.
func (s *SubmissionsService) replyWithheld(ctx context.Context, subs []model.Submission) map[string]bool {
	ids, err := s.repo.Audit.ListSubmissionIDsByEvent(ctx, model.EventReplyDisabled, 0)
	if err != nil {
		s.log.WithError(err).Warn("reply-disabled lookup failed; digest omits the withheld section")
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	disabled := toSet(ids)
	out := make(map[string]bool, len(ids))
	for i := range subs {
		// a submission that has since been replied to is no longer withheld
		if _, ok := disabled[subs[i].ID]; ok && subs[i].LastReplyAt.IsZero() {
			out[subs[i].ID] = true
		}
	}
	return out
}

func (s *SubmissionsService) firstDigestRecipient() string {
	for _, c := range s.checklists.All() {
		if c.Escalation.DigestRecipient != "" {
			return c.Escalation.DigestRecipient
		}
	}
	return s.cfg.Digest.Recipient
}

// resolveFilenameOnly maps each submission's filename-only item ids to their
// human descriptions, dropping any the checklist still reports missing (e.g. a
// filename match that turned out unreadable is not a satisfied match).
func (s *SubmissionsService) resolveFilenameOnly(subs []model.Submission, byID map[string][]string) map[string][]string {
	if len(byID) == 0 {
		return nil
	}
	out := make(map[string][]string, len(byID))
	for i := range subs {
		sub := &subs[i]
		itemIDs := byID[sub.ID]
		if len(itemIDs) == 0 {
			continue
		}
		cl, ok := s.checklists.Get(sub.PolicyType)
		if !ok {
			continue
		}
		desc := make(map[string]string, len(cl.Required))
		for _, it := range cl.Required {
			desc[it.ID] = it.Description
		}
		missing := make(map[string]bool, len(sub.MissingItems))
		for _, m := range sub.MissingItems {
			missing[m.ID] = true
		}
		var names []string
		for _, id := range itemIDs {
			if missing[id] {
				continue
			}
			if d := desc[id]; d != "" {
				names = append(names, d)
			}
		}
		if len(names) > 0 {
			out[sub.ID] = names
		}
	}
	return out
}

func (s *SubmissionsService) createSubmission(ctx context.Context, req IngestRequest) (*model.Submission, error) {
	policy := inferPolicyType(req.Subject, s.checklists.All())
	threadKey := req.MessageID
	if len(req.References) > 0 {
		threadKey = req.References[0]
	}
	sub := model.NewSubmission(
		uuid.NewString(),
		policy,
		req.Subject,
		req.FromAddress,
		req.FromName,
		threadKey,
		s.now(),
	)
	s.audit(ctx, sub.ID, model.EventStateTransitioned, map[string]any{
		"from": "",
		"to":   string(sub.State),
		"note": "created",
	})
	return &sub, nil
}

func (s *SubmissionsService) classifyAttachments(ctx context.Context, sub *model.Submission, e model.Email, cl model.Checklist) []model.Document {
	itemByID := make(map[string]model.RequiredItem, len(cl.Required))
	for _, item := range cl.Required {
		itemByID[item.ID] = item
	}
	out := make([]model.Document, 0, len(e.Attachments))
	for _, a := range e.Attachments {
		out = append(out, s.classifyAttachment(ctx, sub, e, cl, a, itemByID))
	}
	return out
}

// classifyAttachment classifies one attachment and extracts its declared field.
func (s *SubmissionsService) classifyAttachment(ctx context.Context, sub *model.Submission, e model.Email, cl model.Checklist, a model.Attachment, itemByID map[string]model.RequiredItem) model.Document {
	text := s.extractText(a)
	// a password-protected PDF (no extractable text) is the broker's to resend
	// unlocked, not an agency-side unreadable scan
	encrypted := isEncrypted(a) && len(strings.TrimSpace(text)) == 0
	unreadable := !encrypted && s.isUnreadable(a, text)
	result, err := s.classifier.Classify(ctx, classifier.Input{
		Filename:    a.Filename,
		ContentType: a.ContentType,
		BodyText:    text,
		PolicyType:  cl.PolicyType,
		Checklist:   cl,
	})
	if result.Capped {
		s.auditCapOnce(ctx)
	}
	if err != nil {
		s.audit(ctx, sub.ID, model.EventLLMFailed, map[string]any{
			"filename": a.Filename,
			"op":       "classify",
			"error":    err.Error(),
		})
	}
	if result.Usage != nil {
		s.auditLLMCall(ctx, sub.ID, "classify", a.Filename, *result.Usage)
	}

	doc := model.Document{
		ID:            uuid.NewString(),
		SubmissionID:  sub.ID,
		EmailID:       e.DeterministicID,
		Filename:      a.Filename,
		ContentType:   a.ContentType,
		SizeBytes:     a.Size,
		SHA256:        a.SHA256,
		ClassifiedAs:  result.CandidateID,
		Confidence:    result.Confidence,
		ClassifiedBy:  result.By,
		ExtractedText: textutil.TruncateBytes(text, maxExtractedTextBytes),
		Unreadable:    unreadable,
		Encrypted:     encrypted,
		CreatedAt:     s.now(),
	}

	s.audit(ctx, sub.ID, model.EventDocumentClassified, map[string]any{
		"filename":      a.Filename,
		"classified_as": result.CandidateID,
		"confidence":    result.Confidence,
		"by":            result.By,
		"reason":        result.Reason,
	})

	if unreadable {
		s.log.WithField("filename", a.Filename).Warn("document appears unreadable (scanned); no text extracted")
		s.audit(ctx, sub.ID, model.EventDocumentUnreadable, map[string]any{
			"filename":        a.Filename,
			"size_bytes":      a.Size,
			"extracted_chars": len(strings.TrimSpace(text)),
		})
	}
	if encrypted {
		s.log.WithField("filename", a.Filename).Warn("document is password-protected; will ask the broker to resend it unlocked")
		s.audit(ctx, sub.ID, model.EventDocumentEncrypted, map[string]any{
			"filename":   a.Filename,
			"size_bytes": a.Size,
		})
	}

	if item, ok := itemByID[result.CandidateID]; ok && item.RequiresField != nil {
		if extracted := s.extractField(ctx, sub.ID, a.Filename, text, item.RequiresField); extracted != nil {
			doc.ExtractedFields = map[string]any{item.RequiresField.Name: extracted}
		}
	}
	return doc
}

// extractField returns nil if no LLM or the call fails; the checklist then soft-passes.
func (s *SubmissionsService) extractField(ctx context.Context, submissionID, filename, text string, rf *model.RequiresField) any {
	if s.llm == nil {
		return nil
	}
	resp, err := s.llm.ExtractField(ctx, llm.FieldExtractionRequest{
		Filename:         filename,
		TextSample:       text,
		FieldName:        rf.Name,
		FieldDescription: fmt.Sprintf("expected type %s", rf.Type),
		FieldType:        string(rf.Type),
	})
	s.auditLLMCall(ctx, submissionID, "extract_field", filename, resp.Usage)
	if errors.Is(err, llm.ErrSpendCapReached) {
		s.auditCapOnce(ctx)
		return nil // soft-pass: extraction not run
	}
	if err != nil {
		s.audit(ctx, submissionID, model.EventLLMFailed, map[string]any{
			"filename":   filename,
			"op":         "extract_field",
			"field_name": rf.Name,
			"error":      err.Error(),
		})
		return nil
	}
	s.audit(ctx, submissionID, model.EventFieldExtracted, map[string]any{
		"filename":   filename,
		"field_name": rf.Name,
		"value":      resp.Value,
		"confidence": resp.Confidence,
		"reason":     resp.Reason,
	})
	return resp.Value
}

// applicationItemIDs are the checklist items whose document carries the account
// identity (named insured, effective date).
var applicationItemIDs = map[string]bool{
	"acord_125":         true,
	"acord_130":         true,
	"cyber_application": true,
}

var effectiveDateFormats = []string{
	"2006-01-02", "01/02/2006", "1/2/2006", "2006/01/02",
	"January 2, 2006", "Jan 2, 2006", "02-Jan-2006", "2 January 2006",
}

// extractIdentity pulls the named insured and effective date once per submission, from
// a readable application document: LLM, else a "Named Insured" label, else nothing. A
// generic subject is never stored, so content threading can't merge on it.
func (s *SubmissionsService) extractIdentity(ctx context.Context, sub *model.Submission) {
	if sub.NamedInsured != "" {
		return
	}
	doc := applicationDoc(sub.Documents)
	if doc == nil {
		return
	}
	named, effective := "", ""
	if s.llm != nil {
		resp, err := s.llm.ExtractIdentity(ctx, llm.IdentityRequest{Filename: doc.Filename, TextSample: doc.ExtractedText})
		s.auditLLMCall(ctx, sub.ID, "extract_identity", doc.Filename, resp.Usage)
		switch {
		case errors.Is(err, llm.ErrSpendCapReached):
			s.auditCapOnce(ctx)
		case err != nil:
			s.audit(ctx, sub.ID, model.EventLLMFailed, map[string]any{
				"filename": doc.Filename, "op": "extract_identity", "error": err.Error(),
			})
		default:
			named, effective = resp.NamedInsured, resp.EffectiveDate
		}
	}
	if named == "" {
		named = namedInsuredFromText(doc.ExtractedText)
	}
	if named == "" {
		return // never invent one; the digest falls back to the subject line
	}
	sub.NamedInsured = named
	if t, ok := parseEffectiveDate(effective); ok {
		sub.EffectiveDate = &t
	}
	s.audit(ctx, sub.ID, model.EventIdentityExtracted, map[string]any{
		"named_insured":  named,
		"effective_date": effective,
	})
}

// applicationDoc returns a readable application document to extract identity from.
func applicationDoc(docs []model.Document) *model.Document {
	for i := range docs {
		d := &docs[i]
		if applicationItemIDs[d.ClassifiedAs] && !d.Unreadable && !d.Encrypted && strings.TrimSpace(d.ExtractedText) != "" {
			return d
		}
	}
	return nil
}

// namedInsuredFromText reads the value on or after a "Named Insured" label the way
// it appears on ACORD forms; returns "" when the label isn't present.
func namedInsuredFromText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		idx := strings.Index(strings.ToLower(line), "named insured")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line[idx+len("named insured"):]), ":.-# \t"))
		if v := cleanInsured(rest); v != "" {
			return v
		}
		// the value often sits on the next line under the label
		for j := i + 1; j < len(lines) && j <= i+2; j++ {
			if v := cleanInsured(strings.TrimSpace(lines[j])); v != "" {
				return v
			}
		}
	}
	return ""
}

// cleanInsured rejects label echoes and out-of-range values that aren't a name.
func cleanInsured(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 120 {
		return ""
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "named insured") || strings.Contains(low, "mailing address") {
		return ""
	}
	return s
}

// parseEffectiveDate parses a date string in a handful of common formats.
func parseEffectiveDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, f := range effectiveDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// auditCapOnce emits the llm.capped audit at most once per UTC day. It writes
// directly, bypassing the ingest's audit collector: the spend cap is a global
// daily event that must be recorded even if this ingest later fails to commit.
func (s *SubmissionsService) auditCapOnce(ctx context.Context) {
	day := s.now().UTC().Format("2006-01-02")
	s.cappedMu.Lock()
	if s.cappedDay == day {
		s.cappedMu.Unlock()
		return
	}
	s.cappedDay = day // claim now to dedupe; roll back below if the write fails
	s.cappedMu.Unlock()

	entry := &model.AuditEntry{
		EventType: model.EventLLMCapped,
		Payload:   map[string]any{"day": day},
		RequestID: logger.RequestIDFromContext(ctx),
	}
	if err := s.repo.Audit.Append(ctx, entry); err != nil {
		s.cappedMu.Lock()
		s.cappedDay = "" // let a later ingest retry the audit
		s.cappedMu.Unlock()
		s.log.WithError(err).Warn("llm.capped audit append failed")
	}
}

// warnRepliesDisabledOnce logs the global kill switch a single time per process.
// The per-submission record is the reply.disabled audit; this is just the banner.
func (s *SubmissionsService) warnRepliesDisabledOnce() {
	s.replyDisabledOnce.Do(func() {
		s.log.Warn("REPLIES_ENABLED=false; no broker replies will be queued or sent, and none are resent when it goes back on")
	})
}

func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *SubmissionsService) auditLLMCall(ctx context.Context, submissionID, op, filename string, u llm.Usage) {
	if u.PromptHash == "" && u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	s.audit(ctx, submissionID, model.EventLLMCall, map[string]any{
		"op":                 op,
		"filename":           filename,
		"model":              u.Model,
		"prompt_hash":        u.PromptHash,
		"latency_ms":         u.LatencyMs,
		"input_tokens":       u.InputTokens,
		"output_tokens":      u.OutputTokens,
		"estimated_cost_usd": u.EstimatedCostUSD,
	})
}

// isUnreadable flags a document we can't verify: a type with no extractor (image,
// opaque binary; never text/*), or a PDF big enough to be a form yet near-empty.
func (s *SubmissionsService) isUnreadable(a model.Attachment, text string) bool {
	ct := normalizeContentType(a.ContentType)
	if strings.HasPrefix(ct, "text/") {
		return false
	}
	if _, ok := s.extractors[ct]; !ok {
		return true
	}
	if !isPDF(a.ContentType) {
		return false
	}
	if int64(a.Size) < s.cfg.Document.UnreadableMinSizeBytes {
		return false
	}
	return len(strings.TrimSpace(text)) < s.cfg.Document.UnreadableMinChars
}

// isPDF reports a PDF content type, ignoring any MIME parameters.
func isPDF(contentType string) bool {
	ct := normalizeContentType(contentType)
	return ct == "application/pdf" || ct == "application/x-pdf"
}

// isEncrypted reports whether a is a PDF whose trailer carries an /Encrypt marker. Only
// the trailer is searched: a body containing the literal would otherwise tell the broker
// to unlock a file that was never locked. The caller pairs this with an empty extraction.
func isEncrypted(a model.Attachment) bool {
	if !looksLikePDF(a) {
		return false
	}
	return bytes.Contains(pdfTrailer(a.Content), []byte("/Encrypt"))
}

// looksLikePDF accepts a declared PDF, a .pdf filename, or the %PDF- magic. Sniffed
// because mail clients routinely send PDFs as application/octet-stream.
func looksLikePDF(a model.Attachment) bool {
	if len(a.Content) == 0 {
		return false
	}
	if isPDF(a.ContentType) || strings.HasSuffix(strings.ToLower(a.Filename), pdfExtension) {
		return true
	}
	return bytes.HasPrefix(a.Content, []byte("%PDF-"))
}

// pdfTrailer returns the tail of a PDF, where the trailer dictionary lives.
func pdfTrailer(content []byte) []byte {
	if len(content) <= pdfTrailerScanBytes {
		return content
	}
	return content[len(content)-pdfTrailerScanBytes:]
}

// normalizeContentType strips MIME parameters and lowercases so a lookup keyed on
// the bare media type still matches "text/csv; charset=utf-8".
func normalizeContentType(contentType string) string {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		return mt // ParseMediaType lowercases the media type
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

func (s *SubmissionsService) extractText(a model.Attachment) (text string) {
	if len(a.Content) == 0 {
		return ""
	}
	ct := normalizeContentType(a.ContentType)
	ext, ok := s.extractors[ct]
	if !ok || ext == nil {
		if strings.HasPrefix(ct, "text/") {
			return string(a.Content) // plain text reads directly, no extractor needed
		}
		return ""
	}
	// a malformed document must not crash the poller
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("filename", a.Filename).Warnf("text extraction panicked: %v", r)
			text = ""
		}
	}()
	out, err := ext.Extract(a.Content)
	if err != nil {
		s.log.WithError(err).WithField("filename", a.Filename).Warn("text extraction failed")
		return ""
	}
	return out
}

func (s *SubmissionsService) audit(ctx context.Context, submissionID string, evt model.EventType, payload map[string]any) {
	entry := &model.AuditEntry{
		SubmissionID: submissionID,
		EventType:    evt,
		Payload:      payload,
		RequestID:    logger.RequestIDFromContext(ctx),
	}
	if c, ok := ctx.Value(auditCollectorKey{}).(*auditCollector); ok && c != nil {
		c.entries = append(c.entries, entry)
		return
	}
	if err := s.repo.Audit.Append(ctx, entry); err != nil {
		s.log.WithError(err).Warn("audit append failed")
	}
}

// flushAudit writes the buffered entries. Deferred by the ingest so every exit path
// records what it did, and detached from the caller's context so a shutdown between
// the commit and the flush doesn't discard the trail. Safe to call twice.
func (s *SubmissionsService) flushAudit(ctx context.Context, c *auditCollector) {
	if len(c.entries) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditFlushTimeout)
	defer cancel()
	for _, e := range c.entries {
		if err := s.repo.Audit.Append(writeCtx, e); err != nil {
			s.log.WithError(err).Warn("audit append failed")
		}
	}
	c.entries = nil
}

func computeEmailID(messageID, body string, atts []model.Attachment) string {
	h := sha256.New()
	h.Write([]byte(messageID))
	h.Write([]byte{0})
	h.Write([]byte(body))
	hashes := make([]string, 0, len(atts))
	for _, a := range atts {
		if a.SHA256 != "" {
			hashes = append(hashes, a.SHA256)
			continue
		}
		ah := sha256.Sum256(a.Content)
		hashes = append(hashes, hex.EncodeToString(ah[:]))
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		h.Write([]byte{0})
		h.Write([]byte(hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cleanThreadRefs(messageID, inReplyTo string, references []string) []string {
	raw := append([]string{messageID, inReplyTo}, references...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// inferPolicyType returns the policy type whose name, type, or alias appears earliest in
// text. Earliest wins, not first-checklist-wins: a broker's answer must beat their own
// signature block.
func inferPolicyType(text string, all []model.Checklist) string {
	lower := strings.ToLower(text)
	best, bestAt := model.PolicyTypeUnknown, -1
	consider := func(needle, policy string) {
		if needle == "" {
			return
		}
		at := strings.Index(lower, strings.ToLower(needle))
		if at < 0 || (bestAt >= 0 && at >= bestAt) {
			return
		}
		best, bestAt = policy, at
	}
	for _, c := range all {
		consider(c.Name, c.PolicyType)
		consider(c.PolicyType, c.PolicyType)
	}
	for _, a := range policyAliases {
		consider(a.match, a.policy)
	}
	return best
}

func outboundEmail(r model.Reply, providerMsgID string, now time.Time) model.Email {
	// keyed on the provider Message-ID, which a redelivery reuses: the resend upserts one
	// row while a distinct send records its own. Falls back to content when there is no id.
	key := r.SubmissionID + "\x00" + r.ToAddress + "\x00" + r.Subject + "\x00" + r.BodyText
	if providerMsgID != "" {
		key = r.SubmissionID + "\x00" + providerMsgID
	}
	det := sha256.Sum256([]byte(key))
	return model.Email{
		DeterministicID: hex.EncodeToString(det[:]),
		SubmissionID:    r.SubmissionID,
		Direction:       model.DirectionOutbound,
		// store the sent Message-ID so a DSN bounce (or a broker reply quoting it)
		// threads back to this submission
		MessageID:     providerMsgID,
		ToAddresses:   []string{r.ToAddress},
		Subject:       r.Subject,
		BodyText:      r.BodyText,
		ReceivedAt:    now,
		ProviderMsgID: providerMsgID,
		InReplyTo:     r.InReplyTo,
		References:    r.References,
	}
}
