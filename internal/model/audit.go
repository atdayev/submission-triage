package model

import "time"

// EventType identifies an audited pipeline event.
type EventType string

const (
	EventEmailReceived          EventType = "email.received"
	EventEmailDuplicate         EventType = "email.duplicate"
	EventDocumentClassified     EventType = "document.classified"
	EventDocumentUnreadable     EventType = "document.unreadable"
	EventDocumentEncrypted      EventType = "document.encrypted"
	EventFieldExtracted         EventType = "document.field_extracted"
	EventIdentityExtracted      EventType = "submission.identity_extracted"
	EventChecklistEvaluated     EventType = "checklist.evaluated"
	EventPolicyUnknown          EventType = "submission.policy_unknown"
	EventPolicyResolved         EventType = "submission.policy_resolved"
	EventStateTransitioned      EventType = "submission.state_transitioned"
	EventNeedsReview            EventType = "submission.needs_review"
	EventReplySent              EventType = "reply.sent"
	EventReplyFailed            EventType = "reply.failed"
	EventReplySuppressed        EventType = "reply.suppressed"
	EventReplyBounced           EventType = "reply.bounced"
	EventDeliveryRetried        EventType = "reply.delivery_retried"
	EventReplyDisabled          EventType = "reply.disabled"
	EventMailFileFailed         EventType = "mail.file_failed"
	EventAuthFailed             EventType = "auth.failed"
	EventLLMCall                EventType = "llm.call"
	EventLLMFailed              EventType = "llm.failed"
	EventLLMCapped              EventType = "llm.capped"
	EventEscalated              EventType = "submission.escalated"
	EventClosed                 EventType = "submission.closed"
	EventDigestSent             EventType = "escalation.digest_sent"
	EventThreadMatched          EventType = "submission.thread_matched"
	EventThreadMatchedByContent EventType = "submission.thread_matched_by_content"
	EventThreadAmbiguous        EventType = "submission.thread_ambiguous"
	EventHeld                   EventType = "submission.held"
	EventReleased               EventType = "submission.released"
	EventWorkerPanic            EventType = "worker.panic_recovered"
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
