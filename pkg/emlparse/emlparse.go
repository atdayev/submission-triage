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

const maxMultipartDepth = 32 // bound recursion; crafted mail can nest forever

type Address struct {
	Email string
	Name  string
}

type Header struct {
	Name  string
	Value string
}

type Attachment struct {
	Name        string
	Content     string
	ContentType string
}

type Payload struct {
	MessageID   string
	From        string
	FromFull    Address
	To          string
	ToFull      []Address
	Subject     string
	TextBody    string
	Date        string
	Headers     []Header
	Attachments []Attachment
}

func FromFile(path string) (Payload, error) {
	f, err := os.Open(path)
	if err != nil {
		return Payload{}, err
	}
	defer f.Close()
	return FromReader(f)
}

// FromReader parses an RFC822 message (e.g. a raw message fetched over IMAP)
// into the Payload shape the pipeline consumes.
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
	to := splitAddress(msg.Header.Get("To"))

	text, atts, err := parseBody(msg)
	if err != nil {
		return p, err
	}

	p.MessageID = messageID
	p.From = from.Email
	p.FromFull = from
	p.To = to.Email
	p.ToFull = []Address{to}
	p.Subject = decodeHeader(msg.Header.Get("Subject"))
	p.TextBody = text
	p.Date = date
	p.Headers = []Header{
		{Name: "In-Reply-To", Value: inReplyTo},
		{Name: "References", Value: references},
	}
	p.Attachments = atts
	return p, nil
}

func parseBody(msg *mail.Message) (string, []Attachment, error) {
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		body, derr := decodeBody(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
		if derr != nil {
			return "", nil, derr
		}
		text := toUTF8(body, params["charset"])
		if strings.HasPrefix(mediaType, "text/html") {
			text = stripHTML(text)
		}
		return text, nil, nil
	}

	var text, htmlBody string
	var atts []Attachment
	if err := walkParts(msg.Body, params["boundary"], &text, &htmlBody, &atts, 0); err != nil {
		return text, atts, err
	}
	if text == "" && htmlBody != "" {
		text = stripHTML(htmlBody)
	}
	return text, atts, nil
}

// tolerate per-part errors; only an outer reader failure aborts the walk.
func walkParts(r io.Reader, boundary string, text, htmlBody *string, atts *[]Attachment, depth int) error {
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
			return err
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		disp, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))

		if strings.HasPrefix(partType, "multipart/") {
			// nested-walk failures don't abort the outer walk; recover what we can
			_ = walkParts(part, partParams["boundary"], text, htmlBody, atts, depth+1)
			continue
		}

		body, err := decodeBody(part, part.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			continue
		}

		if disp == "attachment" || partParams["name"] != "" {
			*atts = append(*atts, Attachment{
				Name:        decodeHeader(firstNonEmpty(partParams["name"], part.FileName(), "attachment.bin")),
				Content:     base64.StdEncoding.EncodeToString(body),
				ContentType: partType,
			})
			continue
		}
		switch {
		case *text == "" && strings.HasPrefix(partType, "text/plain"):
			*text = toUTF8(body, partParams["charset"])
		case *htmlBody == "" && strings.HasPrefix(partType, "text/html"):
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

// decodeHeader decodes RFC2047 encoded-words; plain text passes through.
func decodeHeader(s string) string {
	got, err := (&mime.WordDecoder{}).DecodeHeader(s)
	if err != nil {
		return s
	}
	return got
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
