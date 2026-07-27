package model

import "time"

// EventType identifies an audited pipeline event.
type EventType string

const (
	EventEmailReceived      EventType = "email.received"
	EventEmailDuplicate     EventType = "email.duplicate"
	EventDocumentClassified EventType = "document.classified"
	EventDocumentUnreadable EventType = "document.unreadable"
	EventFieldExtracted     EventType = "document.field_extracted"
	EventChecklistEvaluated EventType = "checklist.evaluated"
	EventPolicyUnknown      EventType = "submission.policy_unknown"
	EventStateTransitioned  EventType = "submission.state_transitioned"
	EventNeedsReview        EventType = "submission.needs_review"
	EventReplySent          EventType = "reply.sent"
	EventReplyFailed        EventType = "reply.failed"
	EventReplySuppressed    EventType = "reply.suppressed"
	EventMailFileFailed     EventType = "mail.file_failed"
	EventAuthFailed         EventType = "auth.failed"
	EventLLMCall            EventType = "llm.call"
	EventLLMFailed          EventType = "llm.failed"
	EventLLMCapped          EventType = "llm.capped"
	EventEscalated          EventType = "submission.escalated"
	EventClosed             EventType = "submission.closed"
	EventDigestSent         EventType = "escalation.digest_sent"
	EventThreadMatched      EventType = "submission.thread_matched"
	EventThreadAmbiguous    EventType = "submission.thread_ambiguous"
	EventWorkerPanic        EventType = "worker.panic_recovered"
)

// AuditEntry is one recorded audit-log event.
type AuditEntry struct {
	ID           string
	SubmissionID string
	EventType    EventType
	Payload      map[string]any
	RequestID    string
	CreatedAt    time.Time
}
