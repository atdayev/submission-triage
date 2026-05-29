package model

import "time"

type EventType string

const (
	EventEmailReceived      EventType = "email.received"
	EventEmailDuplicate     EventType = "email.duplicate"
	EventDocumentClassified EventType = "document.classified"
	EventFieldExtracted     EventType = "document.field_extracted"
	EventChecklistEvaluated EventType = "checklist.evaluated"
	EventPolicyUnknown      EventType = "submission.policy_unknown"
	EventStateTransitioned  EventType = "submission.state_transitioned"
	EventReplySent          EventType = "reply.sent"
	EventReplyFailed        EventType = "reply.failed"
	EventLLMCall            EventType = "llm.call"
	EventLLMFailed          EventType = "llm.failed"
	EventEscalated          EventType = "submission.escalated"
	EventClosed             EventType = "submission.closed"
	EventDigestSent         EventType = "escalation.digest_sent"
	EventThreadMatched      EventType = "submission.thread_matched"
	EventThreadAmbiguous    EventType = "submission.thread_ambiguous"
)

type AuditEntry struct {
	ID           string
	SubmissionID string
	EventType    EventType
	Payload      map[string]any
	RequestID    string
	CreatedAt    time.Time
}
