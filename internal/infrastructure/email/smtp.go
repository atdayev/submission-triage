package email

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/retry"
)

const smtpDialTimeout = 20 * time.Second
const smtpOpTimeout = 30 * time.Second // whole-exchange deadline; net/smtp has no context

const (
	maxRefs          = 50  // cap References ids
	maxSubjectRunes  = 512 // cap Subject
	headerFoldOctets = 900 // fold below the RFC 5322 998-octet line limit
	maxMsgIDLen      = 256 // cap a message-id token
)

// smtpSendFn performs the network send; swapped out in tests.
type smtpSendFn func(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error

// SMTPSender sends threaded replies over SMTP with retries.
type SMTPSender struct {
	cfg           config.SMTPConfig
	send          smtpSendFn
	log           *logrus.Entry
	retryAttempts int
	retryBase     time.Duration
}

// NewSMTPSender returns an SMTPSender using the real network send.
func NewSMTPSender(cfg config.SMTPConfig, retryAttempts int, retryBase time.Duration, log *logrus.Entry) *SMTPSender {
	return &SMTPSender{
		cfg:           cfg,
		send:          realSMTPSend,
		log:           log,
		retryAttempts: retryAttempts,
		retryBase:     retryBase,
	}
}

// Name reports the outbound channel.
func (s *SMTPSender) Name() string { return "smtp" }

// SendThreadedReply sends r over SMTP with retries and returns the Message-ID.
func (s *SMTPSender) SendThreadedReply(ctx context.Context, r model.Reply) (string, error) {
	if s.cfg.Host == "" {
		return "", errors.New("smtp: host not configured")
	}
	if s.cfg.FromAddress == "" {
		return "", errors.New("smtp: from address not configured")
	}
	if r.ToAddress == "" {
		return "", errors.New("smtp: empty recipient")
	}

	// deterministic Message-ID: an outbox redelivery of the same reply reuses it,
	// so a receiver dedupes the resend instead of treating it as a new message
	msgID := fmt.Sprintf("%s@%s", replyMessageID(r), domainOf(s.cfg.FromAddress))
	msg := s.buildMessage(r, msgID)

	err := retry.Do(ctx, s.retryAttempts, s.retryBase, func(ctx context.Context) error {
		return s.send(ctx, s.cfg, s.cfg.FromAddress, []string{r.ToAddress}, msg)
	})
	if err != nil {
		return "", err
	}
	s.log.WithField("message_id", msgID).Info("smtp reply sent")
	return msgID, nil
}

// buildMessage renders a threaded text/plain RFC822 message.
func (s *SMTPSender) buildMessage(r model.Reply, msgID string) []byte {
	fromAddr := stripCRLF(s.cfg.FromAddress)
	from := fromAddr
	if s.cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", s.cfg.FromName), fromAddr)
	}

	var h strings.Builder
	fmt.Fprintf(&h, "From: %s\r\n", from)
	fmt.Fprintf(&h, "To: %s\r\n", stripCRLF(r.ToAddress))
	fmt.Fprintf(&h, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", truncateRunes(r.Subject, maxSubjectRunes)))
	fmt.Fprintf(&h, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&h, "Message-ID: <%s>\r\n", msgID)
	if id := sanitizeMsgID(r.InReplyTo); id != "" && len(id) <= maxMsgIDLen {
		fmt.Fprintf(&h, "In-Reply-To: <%s>\r\n", id)
	}
	if refs := buildRefs(r.References); refs != "" {
		fmt.Fprintf(&h, "References: %s\r\n", refs)
	}
	h.WriteString("MIME-Version: 1.0\r\n")
	h.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	h.WriteString("\r\n")

	body := strings.ReplaceAll(r.BodyText, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	return []byte(h.String() + body)
}

// stripCRLF removes CR/LF so a crafted field can't inject extra headers.
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

var msgIDStripper = strings.NewReplacer("\r", "", "\n", "", "\t", "", " ", "", "<", "", ">", "")

// sanitizeMsgID reduces a raw reference to a single bare msg-id token, dropping
// whitespace and angle brackets.
func sanitizeMsgID(s string) string {
	return msgIDStripper.Replace(strings.TrimSpace(s))
}

// buildRefs renders the References value from the most recent ids, sanitized and
// folded so each line stays within the RFC 5322 998-octet limit.
func buildRefs(references []string) string {
	if len(references) > maxRefs {
		references = references[len(references)-maxRefs:]
	}
	tokens := make([]string, 0, len(references))
	for _, ref := range references {
		if id := sanitizeMsgID(ref); id != "" && len(id) <= maxMsgIDLen {
			tokens = append(tokens, "<"+id+">")
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := len("References: ")
	for i, t := range tokens {
		switch {
		case i == 0:
		case lineLen+1+len(t) > headerFoldOctets:
			b.WriteString("\r\n ")
			lineLen = 1
		default:
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(t)
		lineLen += len(t)
	}
	return b.String()
}

// truncateRunes caps s to max runes without splitting a multi-byte rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max])
}

// isLocalhost reports loopback hosts.
func isLocalhost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func domainOf(addr string) string {
	if at := strings.LastIndex(addr, "@"); at >= 0 && at < len(addr)-1 {
		return addr[at+1:]
	}
	return "localhost"
}

// replyMessageID derives a stable Message-ID local part from the reply's
// identity so a redelivery of the same reply carries the same id.
func replyMessageID(r model.Reply) string {
	sum := sha256.Sum256([]byte(r.SubmissionID + "\x00" + r.ToAddress + "\x00" + r.Subject + "\x00" + r.BodyText))
	return hex.EncodeToString(sum[:16])
}

func realSMTPSend(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error {
	client, cleanup, err := dialSMTP(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := client.Mail(from); err != nil {
		return classifySMTP(err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return classifySMTP(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return classifySMTP(err)
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return classifySMTP(err)
	}
	// message accepted at Close; a failed QUIT must not trigger a resend
	_ = client.Quit()
	return nil
}

// dialSMTP dials, negotiates implicit TLS or STARTTLS, authenticates, and
// returns the connected client plus a cleanup that closes it.
func dialSMTP(ctx context.Context, cfg config.SMTPConfig) (*smtp.Client, func(), error) {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	tlsCfg := &tls.Config{ServerName: cfg.Host}

	d := &net.Dialer{Timeout: smtpDialTimeout}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	// deadline caps a stalled exchange; close-on-cancel unblocks an in-flight call at shutdown
	_ = rawConn.SetDeadline(time.Now().Add(smtpOpTimeout))
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rawConn.Close()
		case <-stop:
		}
	}()

	// 465 is implicit TLS; other ports upgrade via STARTTLS below
	var conn net.Conn = rawConn
	if cfg.Port == "465" {
		conn = tls.Client(rawConn, tlsCfg)
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = rawConn.Close()
		close(stop)
		return nil, nil, err
	}
	cleanup := func() {
		client.Close()
		close(stop)
	}

	if cfg.Port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				cleanup()
				return nil, nil, err
			}
		} else if !isLocalhost(cfg.Host) {
			cleanup()
			return nil, nil, retry.MarkPermanent(fmt.Errorf("smtp: %s does not offer STARTTLS; refusing to send over plaintext", cfg.Host))
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			cleanup()
			return nil, nil, retry.MarkPermanent(err) // bad credentials won't fix on retry
		}
	}
	return client, cleanup, nil
}

// classifySMTP marks 5xx permanent; other errors stay transient for retry.
func classifySMTP(err error) error {
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 && tp.Code < 600 {
		return retry.MarkPermanent(err)
	}
	return err
}
