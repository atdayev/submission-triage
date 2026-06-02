package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/retry"
)

const smtpDialTimeout = 20 * time.Second
const smtpOpTimeout = 30 * time.Second // whole-exchange deadline; net/smtp has no context

// smtpSendFn performs the network send; swapped out in tests.
type smtpSendFn func(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error

type SMTPSender struct {
	cfg           config.SMTPConfig
	send          smtpSendFn
	log           *logrus.Entry
	retryAttempts int
	retryBase     time.Duration
}

func NewSMTPSender(cfg config.SMTPConfig, retryAttempts int, retryBase time.Duration, log *logrus.Entry) *SMTPSender {
	return &SMTPSender{
		cfg:           cfg,
		send:          realSMTPSend,
		log:           log,
		retryAttempts: retryAttempts,
		retryBase:     retryBase,
	}
}

func (s *SMTPSender) Name() string { return "smtp" }

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

	// one Message-ID for all retries: a resend is the same message
	msgID := fmt.Sprintf("%s@%s", uuid.NewString(), domainOf(s.cfg.FromAddress))
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
	fmt.Fprintf(&h, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", r.Subject))
	fmt.Fprintf(&h, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&h, "Message-ID: <%s>\r\n", msgID)
	if r.InReplyTo != "" {
		fmt.Fprintf(&h, "In-Reply-To: <%s>\r\n", stripCRLF(strings.Trim(r.InReplyTo, "<>")))
	}
	if len(r.References) > 0 {
		refs := make([]string, 0, len(r.References))
		for _, ref := range r.References {
			refs = append(refs, "<"+stripCRLF(strings.Trim(ref, "<>"))+">")
		}
		fmt.Fprintf(&h, "References: %s\r\n", strings.Join(refs, " "))
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

func realSMTPSend(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	tlsCfg := &tls.Config{ServerName: cfg.Host}

	d := &net.Dialer{Timeout: smtpDialTimeout}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// deadline caps a stalled exchange; close-on-cancel unblocks an in-flight call at shutdown
	_ = rawConn.SetDeadline(time.Now().Add(smtpOpTimeout))
	stop := make(chan struct{})
	defer close(stop)
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
		return err
	}
	defer client.Close()

	if cfg.Port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if !isLocalhost(cfg.Host) {
			return retry.MarkPermanent(fmt.Errorf("smtp: %s does not offer STARTTLS; refusing to send over plaintext", cfg.Host))
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return retry.MarkPermanent(err) // bad credentials won't fix on retry
		}
	}
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
	return client.Quit()
}

// classifySMTP marks 5xx permanent; other errors stay transient for retry.
func classifySMTP(err error) error {
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 && tp.Code < 600 {
		return retry.MarkPermanent(err)
	}
	return err
}
