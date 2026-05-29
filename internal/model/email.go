package model

import "time"

type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
)

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

type Attachment struct {
	Filename    string
	ContentType string
	Size        int
	SHA256      string
	Content     []byte
}
