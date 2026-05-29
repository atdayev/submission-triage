package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/apperror"
	"github.com/atdayev/submission-triage/pkg/logger"
	"github.com/atdayev/submission-triage/pkg/postmarkeml"
	"github.com/atdayev/submission-triage/pkg/utils"
)

const (
	webhookSecretHeader    = "X-Webhook-Secret"
	webhookSignatureHeader = "X-Webhook-Signature"
)

type WebhookHandler struct {
	svc *service.SubmissionsService
	cfg config.PostmarkConfig
	log *logrus.Entry
}

func NewWebhookHandler(svc *service.SubmissionsService, cfg config.PostmarkConfig, log *logrus.Entry) *WebhookHandler {
	return &WebhookHandler{svc: svc, cfg: cfg, log: log}
}

func (h *WebhookHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.GetLoggerFromContext(ctx)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, utils.MaxInboundBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			utils.WriteJSONError(w, r, http.StatusRequestEntityTooLarge,
				apperror.NewErrorResponse(apperror.CodeInvalidPayload, "request body too large"))
			return
		}
		utils.WriteJSONError(w, r, http.StatusBadRequest,
			apperror.NewErrorResponse(apperror.CodeInvalidPayload, "read body: "+err.Error()))
		return
	}
	if len(body) == 0 {
		utils.WriteJSONError(w, r, http.StatusBadRequest,
			apperror.NewErrorResponse(apperror.CodeInvalidPayload, "empty body"))
		return
	}

	if !h.authenticate(r, body) {
		utils.WriteJSONError(w, r, http.StatusUnauthorized,
			apperror.NewErrorResponse(apperror.CodeUnauthorized, "invalid webhook credentials"))
		return
	}

	var payload postmarkeml.Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.WithError(err).Warn("decode postmark payload")
		utils.WriteJSONError(w, r, http.StatusBadRequest,
			apperror.NewErrorResponse(apperror.CodeInvalidPayload, "decode body: "+err.Error()))
		return
	}

	result, err := h.svc.IngestEmail(ctx, translate(payload))
	if err != nil {
		log.WithError(err).Error("ingest failed")
		utils.WriteJSONError(w, r, http.StatusInternalServerError,
			apperror.NewErrorResponse(apperror.CodeInternal, "ingest failed"))
		return
	}

	utils.WriteJSON(w, r, http.StatusOK, map[string]any{
		"submission_id": result.SubmissionID,
		"state":         string(result.State),
		"duplicate":     result.IsDuplicate,
		"missing":       result.MissingItems,
		"reply_queued":  result.ReplyQueued,
	})
}

// passes if nothing's configured, or if any configured mechanism checks out.
func (h *WebhookHandler) authenticate(r *http.Request, body []byte) bool {
	sharedConfigured := h.cfg.WebhookSecret != ""
	sigConfigured := h.cfg.WebhookSignatureSecret != ""

	if !sharedConfigured && !sigConfigured {
		return true
	}

	if sharedConfigured {
		provided := r.Header.Get(webhookSecretHeader)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.WebhookSecret)) == 1 {
			return true
		}
	}
	if sigConfigured {
		if verifyHMAC(h.cfg.WebhookSignatureSecret, body, r.Header.Get(webhookSignatureHeader)) {
			return true
		}
	}
	return false
}

// constant-time check of a hex HMAC-SHA256 of the body; "sha256=" prefix optional.
func verifyHMAC(secret string, body []byte, provided string) bool {
	if provided == "" {
		return false
	}
	provided = strings.TrimPrefix(provided, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func translate(p postmarkeml.Payload) service.IngestRequest {
	var inReplyTo string
	var references []string
	for _, hdr := range p.Headers {
		switch strings.ToLower(hdr.Name) {
		case "in-reply-to":
			inReplyTo = strings.TrimSpace(hdr.Value)
		case "references":
			for _, v := range strings.Fields(hdr.Value) {
				references = append(references, strings.TrimSpace(v))
			}
		}
	}

	to := make([]string, 0, len(p.ToFull))
	for _, t := range p.ToFull {
		to = append(to, t.Email)
	}
	if len(to) == 0 && p.To != "" {
		to = []string{p.To}
	}

	receivedAt := time.Now().UTC()
	if t, err := time.Parse(time.RFC1123Z, p.Date); err == nil {
		receivedAt = t.UTC()
	}

	atts := make([]model.Attachment, 0, len(p.Attachments))
	for _, a := range p.Attachments {
		raw, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		atts = append(atts, model.Attachment{
			Filename:    a.Name,
			ContentType: a.ContentType,
			Size:        len(raw),
			SHA256:      hex.EncodeToString(sum[:]),
			Content:     raw,
		})
	}

	fromAddr := p.FromFull.Email
	if fromAddr == "" {
		fromAddr = p.From
	}

	return service.IngestRequest{
		MessageID:   trimAngle(p.MessageID),
		InReplyTo:   trimAngle(inReplyTo),
		References:  trimAngles(references),
		FromAddress: fromAddr,
		FromName:    p.FromFull.Name,
		ToAddresses: to,
		Subject:     p.Subject,
		BodyText:    p.TextBody,
		ReceivedAt:  receivedAt,
		Attachments: atts,
	}
}

func trimAngle(s string) string {
	return strings.Trim(s, "<>")
}

func trimAngles(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = trimAngle(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
