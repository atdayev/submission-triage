package model

import "time"

// Direction is whether an email is inbound or outbound.
type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
)

// Email is a stored inbound or outbound message.
type Email struct {
	DeterministicID string
	SubmissionID    string
	Direction       Direction
	MessageID       string
	InReplyTo       string
	References      []string
	FromAddress     string
	FromName        string
	ToAddresses     []string
	Subject         string
	BodyText        string
	ReceivedAt      time.Time
	ProviderMsgID   string
	Attachments     []Attachment
}

// Attachment is a decoded file attached to an email.
type Attachment struct {
	Filename    string
	ContentType string
	Size        int
	SHA256      string
	Content     []byte
}
