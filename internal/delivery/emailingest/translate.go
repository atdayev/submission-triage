// Package emailingest maps a parsed email payload onto the channel-agnostic IngestRequest.
package emailingest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/mail"
	"strings"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/emlparse"
)

// Translate converts a parsed payload into an IngestRequest stamped with source.
func Translate(p emlparse.Payload, source string) service.IngestRequest {
	var inReplyTo string
	var references []string
	for _, hdr := range p.Headers {
		switch strings.ToLower(hdr.Name) {
		case "in-reply-to":
			inReplyTo = strings.TrimSpace(hdr.Value)
		case "references":
			references = append(references, splitReferences(hdr.Value)...)
		}
	}

	to := make([]string, 0, len(p.ToFull))
	for _, t := range p.ToFull {
		if t.Email != "" {
			to = append(to, t.Email)
		}
	}
	if len(to) == 0 && p.To != "" {
		to = []string{p.To}
	}

	receivedAt := parseReceivedAt(p.Date)

	atts := decodeAttachments(p.Attachments)

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

// parseReceivedAt parses the Date header to UTC, falling back to now on error or unresolved zone.
func parseReceivedAt(date string) time.Time {
	if t, err := mail.ParseDate(date); err == nil && !unresolvedZone(t) {
		return t.UTC()
	}
	return time.Now().UTC()
}

// decodeAttachments base64-decodes each attachment, skipping empties, and stamps a SHA256.
func decodeAttachments(in []emlparse.Attachment) []model.Attachment {
	atts := make([]model.Attachment, 0, len(in))
	for _, a := range in {
		raw, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil || len(raw) == 0 {
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
	return atts
}

func trimAngle(s string) string {
	return strings.Trim(s, "<>")
}

// unresolvedZone reports a named zone that ParseDate stamped at +0000.
func unresolvedZone(t time.Time) bool {
	name, off := t.Zone()
	return off == 0 && name != "" && name != "UTC" && name != "GMT"
}

// splitReferences extracts each <msg-id>, falling back to message-id-like fields.
func splitReferences(v string) []string {
	var out []string
	rest := v
	for {
		i := strings.IndexByte(rest, '<')
		if i < 0 {
			break
		}
		rest = rest[i:]
		j := strings.IndexByte(rest, '>')
		if j < 0 {
			break
		}
		out = append(out, rest[:j+1])
		rest = rest[j+1:]
	}
	if len(out) == 0 {
		for _, f := range strings.Fields(v) {
			if strings.Contains(f, "@") && !strings.ContainsAny(f, "<>") {
				out = append(out, f)
			}
		}
	}
	return out
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
