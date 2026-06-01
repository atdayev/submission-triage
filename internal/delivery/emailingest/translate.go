// Package emailingest maps a parsed email payload onto the channel-agnostic IngestRequest.
package emailingest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/emlparse"
)

// Translate converts a parsed payload into an IngestRequest, stamping the
// inbound channel as source.
func Translate(p emlparse.Payload, source string) service.IngestRequest {
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
		Source:      source,
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
