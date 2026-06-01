package email

import (
	"context"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

type Sender interface {
	SendThreadedReply(ctx context.Context, r model.Reply) (providerMessageID string, err error)
	// Name reports the outbound channel, recorded as "via" on reply.sent audits.
	Name() string
}

// LogSender writes the reply to the log and sends nothing. Dev/demo only.
type LogSender struct {
	log *logrus.Entry
}

func NewLogSender(log *logrus.Entry) *LogSender {
	return &LogSender{log: log}
}

func (s *LogSender) Name() string { return "log" }

func (s *LogSender) SendThreadedReply(_ context.Context, r model.Reply) (string, error) {
	s.log.WithFields(logrus.Fields{
		"to":          r.ToAddress,
		"subject":     r.Subject,
		"in_reply_to": r.InReplyTo,
		"references":  r.References,
	}).Info("log-sender: would send reply")
	return "log-sender-" + uuid.NewString(), nil
}
