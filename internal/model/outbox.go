package model

import "time"

// OutboxStatus is the delivery state of a queued reply.
type OutboxStatus string

const (
	OutboxPending OutboxStatus = "pending"
	OutboxSent    OutboxStatus = "sent"
	OutboxFailed  OutboxStatus = "failed" // dead-lettered after max attempts
)

// OutboxEntry is a reply persisted for durable, at-least-once delivery.
type OutboxEntry struct {
	ID           string
	SubmissionID string
	Reply        Reply
	Status       OutboxStatus
	Attempts     int
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
