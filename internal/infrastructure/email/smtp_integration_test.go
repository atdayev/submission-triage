package email

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
)

// This test runs the real realSMTPSend against a minimal in-process SMTP
// server, exercising the EHLO/AUTH/MAIL/RCPT/DATA sequence. net/smtp allows
// PLAIN auth over plaintext to 127.0.0.1, so no TLS certs are needed.

type receivedMail struct {
	authed   bool
	mailFrom string
	rcpt     []string
	data     string
	done     chan struct{}
}

func fakeSMTPServer(t *testing.T) (string, *receivedMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	rec := &receivedMail{done: make(chan struct{})}
	go func() {
		defer close(rec.done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 test ESMTP\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				// advertise AUTH PLAIN but not STARTTLS, so the client auths without upgrading
				fmt.Fprint(conn, "250-test\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(cmd, "AUTH"):
				rec.authed = true
				fmt.Fprint(conn, "235 2.7.0 accepted\r\n")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				rec.mailFrom = strings.TrimSpace(line)
				fmt.Fprint(conn, "250 ok\r\n")
			case strings.HasPrefix(cmd, "RCPT TO"):
				rec.rcpt = append(rec.rcpt, strings.TrimSpace(line))
				fmt.Fprint(conn, "250 ok\r\n")
			case strings.HasPrefix(cmd, "DATA"):
				fmt.Fprint(conn, "354 end with <CRLF>.<CRLF>\r\n")
				var sb strings.Builder
				for {
					dl, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if dl == ".\r\n" {
						break
					}
					sb.WriteString(dl)
				}
				rec.data = sb.String()
				fmt.Fprint(conn, "250 queued\r\n")
			case strings.HasPrefix(cmd, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "250 ok\r\n")
			}
		}
	}()
	return ln.Addr().String(), rec
}

func TestIntegration_SMTP_RealSendSequence(t *testing.T) {
	addr, rec := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)

	s := &SMTPSender{
		cfg: config.SMTPConfig{
			Host:        host,
			Port:        port,
			Username:    "ops@agency.example",
			Password:    "app-password",
			FromAddress: "ops@agency.example",
			FromName:    "Triage",
		},
		send:          realSMTPSend,
		log:           logrus.NewEntry(logrus.New()),
		retryAttempts: 1,
		retryBase:     time.Millisecond,
	}

	id, err := s.SendThreadedReply(context.Background(), model.Reply{
		ToAddress:  "broker@example.com",
		Subject:    "Re: submission",
		BodyText:   "still need the loss runs",
		InReplyTo:  "root@example.com",
		References: []string{"root@example.com"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id == "" {
		t.Error("empty message id")
	}

	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish the conversation")
	}

	if !rec.authed {
		t.Error("server never saw AUTH")
	}
	if !strings.Contains(rec.mailFrom, "ops@agency.example") {
		t.Errorf("MAIL FROM: %q", rec.mailFrom)
	}
	if len(rec.rcpt) != 1 || !strings.Contains(rec.rcpt[0], "broker@example.com") {
		t.Errorf("RCPT TO: %v", rec.rcpt)
	}
	for _, want := range []string{
		"Subject: Re: submission",
		"In-Reply-To: <root@example.com>",
		"Message-ID: <" + id + ">",
		"still need the loss runs",
	} {
		if !strings.Contains(rec.data, want) {
			t.Errorf("DATA missing %q\n---\n%s", want, rec.data)
		}
	}
}
