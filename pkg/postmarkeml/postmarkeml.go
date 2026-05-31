package postmarkeml

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	Email string `json:"Email"`
	Name  string `json:"Name"`
}

type Header struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type Attachment struct {
	Name          string `json:"Name"`
	Content       string `json:"Content"`
	ContentType   string `json:"ContentType"`
	ContentLength int    `json:"ContentLength"`
}

type Payload struct {
	MessageID   string       `json:"MessageID"`
	From        string       `json:"From"`
	FromFull    Address      `json:"FromFull"`
	To          string       `json:"To"`
	ToFull      []Address    `json:"ToFull"`
	Subject     string       `json:"Subject"`
	TextBody    string       `json:"TextBody"`
	Date        string       `json:"Date"`
	Headers     []Header     `json:"Headers"`
	Attachments []Attachment `json:"Attachments"`
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
// into the same Payload shape as the Postmark webhook produces.
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
	p.Subject = msg.Header.Get("Subject")
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
		return string(body), nil, nil
	}

	var text string
	var atts []Attachment
	if err := walkParts(msg.Body, params["boundary"], &text, &atts, 0); err != nil {
		return text, atts, err
	}
	return text, atts, nil
}

// tolerate per-part errors; only an outer reader failure aborts the walk.
func walkParts(r io.Reader, boundary string, text *string, atts *[]Attachment, depth int) error {
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
			_ = walkParts(part, partParams["boundary"], text, atts, depth+1)
			continue
		}

		body, err := decodeBody(part, part.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			continue
		}

		if disp == "attachment" || partParams["name"] != "" {
			*atts = append(*atts, Attachment{
				Name:          firstNonEmpty(partParams["name"], part.FileName(), "attachment.bin"),
				Content:       base64.StdEncoding.EncodeToString(body),
				ContentType:   partType,
				ContentLength: len(body),
			})
			continue
		}
		if *text == "" && strings.HasPrefix(partType, "text/plain") {
			*text = string(body)
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
		if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil {
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
