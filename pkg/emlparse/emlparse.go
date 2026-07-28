// Package emlparse parses RFC822 email into the Payload the pipeline consumes.
package emlparse

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"time"
)

const (
	maxMultipartDepth = 32  // bound recursion
	maxAttachments    = 100 // bound per-message attachment count
)

// autoResponseHeaders are surfaced on Payload.Headers so the pipeline can
// detect automated mail (out-of-office, bulk, list, bounces) and record why a
// reply was suppressed. Not every header drives detection (see detectAutoSubmitted);
// X-Auto-Response-Suppress is kept only for the suppression audit trail.
var autoResponseHeaders = []string{
	"Auto-Submitted",
	"Precedence",
	"List-Id",
	"List-Unsubscribe",
	"X-Auto-Response-Suppress",
	"Return-Path",
}

// Address is a parsed email address with optional display name.
type Address struct {
	Email string
	Name  string
}

// Header is a single raw message header.
type Header struct {
	Name  string
	Value string
}

// Attachment is a decoded message part stored as base64 content.
type Attachment struct {
	Name        string
	Content     string
	ContentType string
}

// Payload is the parsed message the pipeline consumes.
type Payload struct {
	MessageID   string
	From        string
	FromFull    Address
	ReplyTo     string
	ReplyToFull Address
	To          string
	ToFull      []Address
	Subject     string
	TextBody    string
	Date        string
	Headers     []Header
	Attachments []Attachment
	// Bounce marks a delivery-status notification (a hard/soft bounce of one of
	// our replies); BouncePermanent is true for a 5.x.x DSN or an unparseable code.
	Bounce          bool
	BouncePermanent bool
}

// FromFile parses an RFC822 message file into a Payload.
func FromFile(path string) (Payload, error) {
	f, err := os.Open(path)
	if err != nil {
		return Payload{}, err
	}
	defer f.Close()
	return FromReader(f)
}

// FromReader parses an RFC822 message into a Payload.
func FromReader(r io.Reader) (Payload, error) {
	var p Payload
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return p, fmt.Errorf("parse eml: %w", err)
	}

	messageID := strings.Trim(msg.Header.Get("Message-ID"), "<>")
	inReplyTo := strings.Trim(msg.Header.Get("In-Reply-To"), "<>")
	references := msg.Header.Get("References")
	date := msg.Header.Get("Date")
	if date == "" {
		date = time.Now().Format(time.RFC1123Z)
	}
	from := splitAddress(msg.Header.Get("From"))
	replyTo := splitAddress(msg.Header.Get("Reply-To"))
	to := splitAddress(msg.Header.Get("To"))

	text, atts, deliveryStatus, err := parseBody(msg)
	if err != nil {
		return p, err
	}

	p.MessageID = messageID
	p.From = from.Email
	p.FromFull = from
	p.ReplyTo = replyTo.Email
	p.ReplyToFull = replyTo
	p.To = to.Email
	p.ToFull = []Address{to}
	p.Subject = decodeHeader(msg.Header.Get("Subject"))
	p.TextBody = text
	p.Date = date
	p.Headers = []Header{
		{Name: "In-Reply-To", Value: inReplyTo},
		{Name: "References", Value: references},
	}
	for _, name := range autoResponseHeaders {
		if v := strings.TrimSpace(msg.Header.Get(name)); v != "" {
			p.Headers = append(p.Headers, Header{Name: name, Value: stripControl(v)})
		}
	}
	p.Attachments = atts

	topType, ctParams, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	p.Bounce, p.BouncePermanent = detectBounce(topType, ctParams, from.Email, msg.Header.Get("Return-Path"), deliveryStatus)
	return p, nil
}

// detectBounce reports whether the message is a delivery-status notification and,
// if so, whether the failure is permanent. Any one of three signals triggers it:
// a multipart/report delivery-status, a mailer-daemon/postmaster sender, or a
// null return-path alongside a message/delivery-status part. A 4.x.x DSN status
// is transient; anything else (including an unparseable code) is permanent.
func detectBounce(mediaType string, ctParams map[string]string, fromEmail, returnPath, deliveryStatus string) (bounce, permanent bool) {
	report := mediaType == "multipart/report" && strings.EqualFold(ctParams["report-type"], "delivery-status")
	nullReturn := strings.TrimSpace(returnPath) == "<>"
	bounce = report || isDaemonAddress(fromEmail) || (nullReturn && deliveryStatus != "")
	if !bounce {
		return false, false
	}
	return true, dsnPermanent(deliveryStatus)
}

func isDaemonAddress(email string) bool {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	switch strings.ToLower(strings.TrimSpace(local)) {
	case "mailer-daemon", "postmaster":
		return true
	}
	return false
}

// dsnPermanent reads the DSN "Status: N.x.x" field; only a 4.x.x class is
// transient, so an absent or unparseable code is treated as permanent.
func dsnPermanent(deliveryStatus string) bool {
	for line := range strings.SplitSeq(deliveryStatus, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "status:") {
			continue
		}
		return !strings.HasPrefix(strings.TrimSpace(line[len("status:"):]), "4.")
	}
	return true
}

func parseBody(msg *mail.Message) (string, []Attachment, string, error) {
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		body, derr := decodeBody(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
		if derr != nil {
			return "", nil, "", derr
		}
		text := toUTF8(body, params["charset"])
		if strings.HasPrefix(mediaType, "text/html") {
			text = stripHTML(text)
		}
		return text, nil, "", nil
	}

	var text, htmlBody, deliveryStatus string
	var atts []Attachment
	if err := walkParts(msg.Body, params["boundary"], &text, &htmlBody, &atts, &deliveryStatus, 0); err != nil {
		return text, atts, deliveryStatus, err
	}
	if text == "" && htmlBody != "" {
		text = stripHTML(htmlBody)
	}
	return text, atts, deliveryStatus, nil
}

// walkParts collects text, HTML, and attachments from a multipart stream,
// recovering what it parsed past per-part or truncated-stream errors.
func walkParts(r io.Reader, boundary string, text, htmlBody *string, atts *[]Attachment, deliveryStatus *string, depth int) error {
	if depth > maxMultipartDepth {
		return fmt.Errorf("multipart: nesting exceeds depth %d", maxMultipartDepth)
	}
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil // truncated or boundary-less stream; keep what we parsed
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if partType == "" {
			partType = "text/plain" // RFC822 default for a typeless part
		}
		disp, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))

		if strings.HasPrefix(partType, "multipart/") {
			// nested-walk failures don't abort the outer walk; recover what we can
			_ = walkParts(part, partParams["boundary"], text, htmlBody, atts, deliveryStatus, depth+1)
			continue
		}

		body, err := decodeBody(part, part.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			continue
		}

		// a delivery-status report part carries the DSN Status code that tells a
		// hard bounce from a transient one
		if partType == "message/delivery-status" && *deliveryStatus == "" {
			*deliveryStatus = string(body)
			continue
		}

		// a named text part fills the body only while its slot is open; once the
		// body is set, a later named text part is kept as an attachment, not dropped
		bodyText := *text == "" && strings.HasPrefix(partType, "text/plain")
		bodyHTML := *htmlBody == "" && strings.HasPrefix(partType, "text/html")
		if disp == "attachment" || (partParams["name"] != "" && !bodyText && !bodyHTML) {
			if len(*atts) >= maxAttachments {
				continue
			}
			*atts = append(*atts, Attachment{
				Name:        decodeHeader(firstNonEmpty(partParams["name"], part.FileName(), "attachment.bin")),
				Content:     base64.StdEncoding.EncodeToString(body),
				ContentType: partType,
			})
			continue
		}
		switch {
		case bodyText:
			*text = toUTF8(body, partParams["charset"])
		case bodyHTML:
			*htmlBody = toUTF8(body, partParams["charset"])
		}
	}
}

func decodeBody(r io.Reader, encoding string) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		if dec, err := base64.StdEncoding.DecodeString(stripASCIISpace(string(raw))); err == nil {
			return dec, nil
		}
		return raw, nil
	case "quoted-printable":
		dec, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(raw))))
		if err != nil {
			return raw, nil
		}
		return dec, nil
	default:
		return raw, nil
	}
}

// decodeHeader decodes RFC2047 encoded-words and strips control characters.
func decodeHeader(s string) string {
	got, err := (&mime.WordDecoder{}).DecodeHeader(s)
	if err != nil {
		got = s
	}
	return stripControl(got)
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func splitAddress(in string) Address {
	if in == "" {
		return Address{}
	}
	addr, err := mail.ParseAddress(in)
	if err != nil {
		return Address{Email: in}
	}
	return Address{Email: addr.Address, Name: addr.Name}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func stripASCIISpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
}

// stripHTML drops tags and unescapes entities.
func stripHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(html.UnescapeString(b.String()))
}
