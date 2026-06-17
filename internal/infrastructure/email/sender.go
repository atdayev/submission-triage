package email

import (
	"context"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

// Sender sends a threaded reply over an outbound channel.
type Sender interface {
	SendThreadedReply(ctx context.Context, r model.Reply) (providerMessageID string, err error)
	// Name reports the outbound channel, recorded as "via" on reply.sent audits.
	Name() string
}

// LogSender writes the reply to the log and sends nothing. Dev/demo only.
type LogSender struct {
	log *logrus.Entry
}

// NewLogSender returns a LogSender writing to log.
func NewLogSender(log *logrus.Entry) *LogSender {
	return &LogSender{log: log}
}

// Name reports the outbound channel.
func (s *LogSender) Name() string { return "log" }

// SendThreadedReply logs r and returns a synthetic message id.
func (s *LogSender) SendThreadedReply(_ context.Context, r model.Reply) (string, error) {
	s.log.WithFields(logrus.Fields{
		"to":          r.ToAddress,
		"subject":     r.Subject,
		"in_reply_to": r.InReplyTo,
		"references":  r.References,
	}).Info("log-sender: would send reply")
	return "log-sender-" + uuid.NewString(), nil
}
