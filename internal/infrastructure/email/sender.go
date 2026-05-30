package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/postmarkeml"
	"github.com/atdayev/submission-triage/pkg/retry"
)

const (
	postmarkAPIURL      = "https://api.postmarkapp.com/email"
	postmarkHTTPTimeout = 20 * time.Second
)

type Sender interface {
	SendThreadedReply(ctx context.Context, r model.Reply) (providerMessageID string, err error)
	// Name reports the outbound channel, recorded as "via" on reply.sent audits.
	Name() string
}

type PostmarkSender struct {
	cfg           config.PostmarkConfig
	httpClient    *http.Client
	endpoint      string
	log           *logrus.Entry
	retryAttempts int
	retryBase     time.Duration
}

type LogSender struct {
	log *logrus.Entry
}

type postmarkSendRequest struct {
	From          string               `json:"From"`
	To            string               `json:"To"`
	Subject       string               `json:"Subject"`
	TextBody      string               `json:"TextBody,omitempty"`
	MessageStream string               `json:"MessageStream"`
	Headers       []postmarkeml.Header `json:"Headers,omitempty"`
}

type postmarkSendResponse struct {
	MessageID string `json:"MessageID"`
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

func NewPostmarkSender(cfg config.PostmarkConfig, retryAttempts int, retryBase time.Duration, log *logrus.Entry) *PostmarkSender {
	return &PostmarkSender{
		cfg:           cfg,
		httpClient:    &http.Client{Timeout: postmarkHTTPTimeout},
		endpoint:      postmarkAPIURL,
		log:           log,
		retryAttempts: retryAttempts,
		retryBase:     retryBase,
	}
}

func NewLogSender(log *logrus.Entry) *LogSender {
	return &LogSender{log: log}
}

func (s *PostmarkSender) Name() string { return "postmark" }

func (s *PostmarkSender) SendThreadedReply(ctx context.Context, r model.Reply) (string, error) {
	if s.cfg.ServerToken == "" {
		return "", errors.New("postmark: server token not configured")
	}
	if r.ToAddress == "" {
		return "", errors.New("postmark: empty recipient")
	}

	payload, err := s.buildPayload(r)
	if err != nil {
		return "", err
	}

	var providerMsgID string
	err = retry.Do(ctx, s.retryAttempts, s.retryBase, func(ctx context.Context) error {
		id, err := s.postOnce(ctx, payload)
		if err != nil {
			return err
		}
		providerMsgID = id
		return nil
	})
	if err != nil {
		return "", err
	}
	s.log.WithField("provider_msg_id", providerMsgID).Info("postmark reply sent")
	return providerMsgID, nil
}

func (s *PostmarkSender) buildPayload(r model.Reply) ([]byte, error) {
	from := s.cfg.FromAddress
	if s.cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", s.cfg.FromName, s.cfg.FromAddress)
	}

	headers := []postmarkeml.Header{}
	if r.InReplyTo != "" {
		headers = append(headers, postmarkeml.Header{Name: "In-Reply-To", Value: r.InReplyTo})
	}
	if len(r.References) > 0 {
		headers = append(headers, postmarkeml.Header{Name: "References", Value: strings.Join(r.References, " ")})
	}

	payload, err := json.Marshal(postmarkSendRequest{
		From:          from,
		To:            r.ToAddress,
		Subject:       r.Subject,
		TextBody:      r.BodyText,
		MessageStream: "outbound",
		Headers:       headers,
	})
	if err != nil {
		return nil, fmt.Errorf("postmark: marshal: %w", err)
	}
	return payload, nil
}

func (s *PostmarkSender) postOnce(ctx context.Context, payload []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", retry.MarkPermanent(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Postmark-Server-Token", s.cfg.ServerToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var parsed postmarkSendResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("postmark: decode: %w", err)
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("postmark: server error %d: %s", resp.StatusCode, parsed.Message)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("postmark: rate limited %d: %s", resp.StatusCode, parsed.Message)
	}
	if resp.StatusCode >= 400 {
		return "", retry.MarkPermanent(fmt.Errorf("postmark: client error %d (%d): %s", resp.StatusCode, parsed.ErrorCode, parsed.Message))
	}
	return parsed.MessageID, nil
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
