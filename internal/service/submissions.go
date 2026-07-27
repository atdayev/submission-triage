package service

import (
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
}

const (
	defaultReplyDrainGrace   = 10 * time.Second
	defaultOutboxRetryAfter  = 2 * time.Minute
	defaultOutboxMaxAttempts = 10
	defaultOutboxBatch       = 100
	markSentTimeout          = 2 * time.Second // detached mark-sent: bounds shutdown overrun
	defaultDigestMaxRows     = 500             // fallback when DIGEST_MAX_ROWS is unset
)

const maxExtractedTextBytes = 256 << 10 // bound stored per-document extracted text

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
	ToAddresses []string
	Subject     string
	BodyText    string
	ReceivedAt  time.Time
	Attachments []model.Attachment
	Source      string
	// AutoSubmitted marks mail from an automated agent (out-of-office, bulk,
	// list, bounce); such mail is stored but never replied to.
	AutoSubmitted bool
	// AutoResponseHeaders are the auto-response headers seen on the message,
	// recorded when a reply is suppressed.
	AutoResponseHeaders map[string]string
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

// auditCollector buffers an ingest's audit entries until the submission commits,
// keeping a failed, retried ingest from leaving orphan rows.
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
	defer s.releaseDispatch(job.outboxID)
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

func (s *SubmissionsService) claimDispatch(id string) {
	s.dispatchMu.Lock()
	if s.dispatching == nil {
		s.dispatching = map[string]struct{}{}
	}
	s.dispatching[id] = struct{}{}
	s.dispatchMu.Unlock()
}

func (s *SubmissionsService) releaseDispatch(id string) {
	s.dispatchMu.Lock()
	delete(s.dispatching, id)
	s.dispatchMu.Unlock()
}

func (s *SubmissionsService) isDispatching(id string) bool {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	_, ok := s.dispatching[id]
	return ok
}

// enqueueReply hands a job to the worker pool. Returns false if the pool is
// shutting down or its queue is full, leaving the row for the outbox sweeper.
func (s *SubmissionsService) enqueueReply(job replyJob) bool {
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closed {
		return false
	}
	s.claimDispatch(job.outboxID)
	s.inFlightWG.Add(1)
	select {
	case s.replyJobs <- job:
		return true
	default:
		s.releaseDispatch(job.outboxID)
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

	sub, dup, err := s.resolveSubmission(ctx, req, emailID)
	if err != nil {
		return IngestResult{}, err
	}
	if dup != nil {
		s.flushAudit(ctx, collector)
		return *dup, nil
	}

	inbound := inboundEmail(req, emailID, sub.ID)
	sub.AttachEmail(inbound)
	// an automated sender's action doesn't advance the escalation clock
	if !req.AutoSubmitted {
		sub.MarkAction(s.now())
	}

	s.audit(ctx, sub.ID, model.EventEmailReceived, map[string]any{
		"deterministic_id": emailID,
		"message_id":       req.MessageID,
		"attachment_count": len(req.Attachments),
		"source":           req.Source,
	})

	missing, policyUnknown := s.evaluate(ctx, sub, inbound)

	// the review flag and the broker reply are decided independently: an unreadable
	// file doesn't suppress the request for the documents that are genuinely absent
	prevReview := sub.NeedsReview
	sub.NeedsReview = model.RequiresReview(missing)
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
		return s.persistSuppressed(ctx, sub, collector, req.AutoResponseHeaders, result)
	}
	reply, sendReply := s.buildReply(sub, missing, inbound, policyUnknown)
	if !sendReply {
		// nothing to ask the broker (flagged-only): persist the submission alone.
		return s.persistNoReply(ctx, sub, collector, result)
	}
	// submission and reply commit in one tx: an inbound email without an outbox
	// row is unrecoverable (dedup blocks the retry). On failure the poller retries.
	now := s.now()
	notBefore := s.replyNotBefore(sub, now)
	entry := &model.OutboxEntry{ID: uuid.NewString(), SubmissionID: sub.ID, Reply: reply, Status: model.OutboxPending, NotBefore: notBefore}
	if err := s.repo.Submissions.UpsertSubmissionWithReply(ctx, sub, entry); err != nil {
		return IngestResult{}, fmt.Errorf("persist submission with reply: %w", err)
	}
	result.ReplyQueued = true
	s.flushAudit(ctx, collector)

	// dispatch online only when due now (the first reply, or once the coalesce
	// window has elapsed); otherwise the flush sweeper sends it after not_before
	if !notBefore.After(now) {
		job := replyJob{ctx: s.replyJobContext(ctx), reply: reply, submissionID: sub.ID, outboxID: entry.ID, version: entry.UpdatedAt}
		_ = s.enqueueReply(job)
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
func (s *SubmissionsService) persistSuppressed(ctx context.Context, sub *model.Submission, collector *auditCollector, headers map[string]string, result IngestResult) (IngestResult, error) {
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("persist suppressed submission: %w", err)
	}
	s.audit(ctx, sub.ID, model.EventReplySuppressed, map[string]any{
		"reason":  "auto_submitted",
		"headers": headers,
	})
	s.flushAudit(ctx, collector)
	return result, nil
}

// persistNoReply commits a submission with no broker reply: everything outstanding
// is flagged agency-side, so there is nothing to ask and no reply to recover.
func (s *SubmissionsService) persistNoReply(ctx context.Context, sub *model.Submission, collector *auditCollector, result IngestResult) (IngestResult, error) {
	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("persist submission: %w", err)
	}
	s.flushAudit(ctx, collector)
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
		ToAddresses:     req.ToAddresses,
		Subject:         req.Subject,
		BodyText:        req.BodyText,
		ReceivedAt:      req.ReceivedAt,
		Attachments:     req.Attachments,
	}
}

// evaluate classifies the inbound attachments, runs the checklist, and records
// the result. policyUnknown means no checklist matched the policy type.
func (s *SubmissionsService) evaluate(ctx context.Context, sub *model.Submission, inbound model.Email) (missing []model.MissingItem, policyUnknown bool) {
	checklistDef, hasChecklist := s.checklists.Get(sub.PolicyType)
	policyUnknown = !hasChecklist
	if policyUnknown {
		sub.PolicyType = model.PolicyTypeUnknown
		s.audit(ctx, sub.ID, model.EventPolicyUnknown, map[string]any{"subject_hint": inbound.Subject})
		missing = []model.MissingItem{{
			ID:          "policy_unknown",
			Description: "Policy type not yet determined",
			Reason:      "awaiting clarification from sender",
		}}
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
	drained := make(chan struct{})
	go func() {
		s.inFlightWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(s.drainGrace):
		s.log.Warn("reply drain exceeded grace; canceling in-flight sends")
		s.replyCancel()
		s.inFlightWG.Wait()
	}
	close(s.replyJobs)
	s.replyWG.Wait()
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
func (s *SubmissionsService) buildReply(sub *model.Submission, missing []model.MissingItem, inbound model.Email, policyUnknown bool) (model.Reply, bool) {
	switch {
	case policyUnknown:
		return model.BuildPolicyUnknownReply(*sub, inbound, s.knownChecklistNames()), true
	case len(missing) == 0:
		return model.BuildCompletionReply(*sub, inbound), true
	default:
		if broker := model.BrokerActionable(missing); len(broker) > 0 {
			return model.BuildMissingItemsReply(*sub, broker, inbound), true
		}
		return model.Reply{}, false
	}
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

// RedeliverOutbox resends pending replies the online path didn't get out
// (overflow, crash, outage), dead-lettering entries after outboxMaxAttempts.
func (s *SubmissionsService) RedeliverOutbox(ctx context.Context) error {
	now := s.now()
	cutoff := now.Add(-s.outboxRetryAfter)
	rows, err := s.repo.Outbox.ListPending(ctx, now, cutoff, s.outboxBatch)
	if err != nil {
		return fmt.Errorf("outbox: list pending: %w", err)
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.isDispatching(row.ID) {
			continue
		}
		if err := s.deliver(ctx, row.Reply, row.SubmissionID); err != nil {
			attempts := row.Attempts + 1
			if attempts >= s.outboxMaxAttempts {
				_ = s.repo.Outbox.Update(ctx, row.ID, model.OutboxFailed, attempts, err.Error())
				s.audit(ctx, row.SubmissionID, model.EventReplyFailed, map[string]any{
					"error": err.Error(), "attempts": attempts, "dead_lettered": true,
				})
			} else {
				_ = s.repo.Outbox.Update(ctx, row.ID, model.OutboxPending, attempts, err.Error())
			}
			continue
		}
		// guard on the version read from ListPending: a follow-up that coalesced
		// into this row since then stays pending for the next sweep, not lost
		_ = s.repo.Outbox.MarkSent(ctx, row.ID, row.UpdatedAt)
	}
	return nil
}

// CheckEscalations escalates submissions idle past their policy's threshold.
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
	cutoff := now.Add(-minThreshold)
	stale, err := s.repo.Submissions.ListStale(ctx, cutoff.UnixNano(), 100)
	if err != nil {
		return fmt.Errorf("list stale: %w", err)
	}
	for i := range stale {
		sub := &stale[i]
		threshold := globalThreshold
		if cl, ok := s.checklists.Get(sub.PolicyType); ok && cl.Escalation.ThresholdHours > 0 {
			threshold = time.Duration(cl.Escalation.ThresholdHours) * time.Hour
		}
		if now.Sub(sub.LastActionAt) < threshold {
			continue
		}
		// only nudge the broker when they can act; a submission blocked solely on
		// an unreadable/low-confidence file is the agency's problem, not theirs.
		if len(model.BrokerActionable(sub.MissingItems)) == 0 {
			continue
		}
		if err := sub.TransitionTo(model.StateEscalated, now); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("could not escalate")
			continue
		}
		if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("escalation upsert failed")
			continue
		}
		s.audit(ctx, sub.ID, model.EventEscalated, map[string]any{
			"last_action_at": sub.LastActionAt,
			"threshold":      threshold.String(),
		})
	}
	return nil
}

// CheckClosures auto-closes completed and escalated submissions gone quiet.
func (s *SubmissionsService) CheckClosures(ctx context.Context) error {
	autoClose := s.cfg.Escalation.AutoCloseAfter()
	if autoClose <= 0 {
		return nil
	}
	now := s.now()
	cutoff := now.Add(-autoClose)

	subs, err := s.repo.Submissions.ListCompletedBefore(ctx, cutoff.UnixNano(), 100)
	if err != nil {
		return fmt.Errorf("list completed-before: %w", err)
	}
	for i := range subs {
		s.closeQuiet(ctx, &subs[i], "auto_close_after_complete", now)
	}

	// also retire escalated cases gone quiet past the same window
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

// SendDigest emails the agency a daily status digest of every open submission.
// No-ops without a recipient/interval, or when nothing is open (an empty digest
// trains the agency to ignore the address).
func (s *SubmissionsService) SendDigest(ctx context.Context) error {
	interval := s.cfg.Digest.Interval()
	recipient := s.firstDigestRecipient()
	if recipient == "" || interval <= 0 {
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
		BodyText:  model.BuildDigest(subs, s.resolveFilenameOnly(subs, flaggedIDs), omitted, s.now()),
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
	unreadable := s.isUnreadable(a, text)
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

// flushAudit writes the buffered entries; called only after a successful commit.
func (s *SubmissionsService) flushAudit(ctx context.Context, c *auditCollector) {
	for _, e := range c.entries {
		if err := s.repo.Audit.Append(ctx, e); err != nil {
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

func inferPolicyType(subject string, all []model.Checklist) string {
	lower := strings.ToLower(subject)
	for _, c := range all {
		if strings.Contains(lower, strings.ToLower(c.Name)) {
			return c.PolicyType
		}
		if strings.Contains(lower, strings.ToLower(c.PolicyType)) {
			return c.PolicyType
		}
	}
	for _, a := range policyAliases {
		if strings.Contains(lower, a.match) {
			return a.policy
		}
	}
	return model.PolicyTypeUnknown
}

func outboundEmail(r model.Reply, providerMsgID string, now time.Time) model.Email {
	// stable across redeliveries so a resend upserts the same row, not a duplicate
	det := sha256.Sum256([]byte(r.SubmissionID + "\x00" + r.ToAddress + "\x00" + r.Subject + "\x00" + r.BodyText))
	return model.Email{
		DeterministicID: hex.EncodeToString(det[:]),
		SubmissionID:    r.SubmissionID,
		Direction:       model.DirectionOutbound,
		ToAddresses:     []string{r.ToAddress},
		Subject:         r.Subject,
		BodyText:        r.BodyText,
		ReceivedAt:      now,
		ProviderMsgID:   providerMsgID,
		InReplyTo:       r.InReplyTo,
		References:      r.References,
	}
}
