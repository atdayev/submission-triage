//go:build integration

package email

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
	"github.com/atdayev/submission-triage/internal/model"
)

// runs realSMTPSend against a minimal in-process SMTP server (EHLO/AUTH/MAIL/RCPT/DATA).

// passwordSend is realSMTPSend with no injected mechanism (password/PlainAuth path).
func passwordSend(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error {
	return realSMTPSend(ctx, cfg, nil, from, to, msg)
}

type receivedMail struct {
	authed      bool
	authMech    string
	authPayload string
	mailFrom    string
	rcpt        []string
	data        string
	done        chan struct{}
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
				// no STARTTLS advertised; the client only proceeds in the clear
				// because the host is loopback (isLocalhost), never for a remote server
				fmt.Fprint(conn, "250-test\r\n250 AUTH PLAIN XOAUTH2\r\n")
			case strings.HasPrefix(cmd, "AUTH"):
				rec.authed = true
				if fields := strings.Fields(strings.TrimSpace(line)); len(fields) >= 2 {
					rec.authMech = strings.ToUpper(fields[1])
					if len(fields) >= 3 {
						if dec, err := base64.StdEncoding.DecodeString(fields[2]); err == nil {
							rec.authPayload = string(dec)
						}
					}
				}
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

// stallingServer accepts a connection then never speaks SMTP, so a client
// blocks reading the greeting — used to prove context cancellation unblocks it.
func stallingServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-stop
	}()
	return ln.Addr().String()
}

func TestIntegration_SMTP_ContextCancelUnblocksSend(t *testing.T) {
	host, port, _ := net.SplitHostPort(stallingServer(t))
	s := &SMTPSender{
		cfg:           config.SMTPConfig{Host: host, Port: port, FromAddress: "ops@agency.example"},
		send:          passwordSend,
		log:           logrus.NewEntry(logrus.New()),
		retryAttempts: 1,
		retryBase:     time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.SendThreadedReply(ctx, model.Reply{ToAddress: "broker@x", Subject: "hi"})
		errCh <- err
	}()

	// let the client connect and block on the greeting, then cancel mid-send
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("send did not return after cancellation; close-on-cancel is broken")
	}
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
		send:          passwordSend,
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

func TestIntegration_SMTP_XOAUTH2(t *testing.T) {
	const (
		user  = "ops@agency.example"
		token = "ya29.stub-access-token"
	)
	addr, rec := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	auth := XOAUTH2Auth(user, stubTokenSource{token: token})
	s := &SMTPSender{
		cfg: config.SMTPConfig{Host: host, Port: port, FromAddress: user, FromName: "Triage"},
		send: func(ctx context.Context, cfg config.SMTPConfig, from string, to []string, msg []byte) error {
			return realSMTPSend(ctx, cfg, auth, from, to, msg)
		},
		log:           logrus.NewEntry(logrus.New()),
		retryAttempts: 1,
		retryBase:     time.Millisecond,
	}

	if _, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@example.com", Subject: "hi", BodyText: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish the conversation")
	}

	if rec.authMech != "XOAUTH2" {
		t.Errorf("auth mechanism: got %q, want XOAUTH2", rec.authMech)
	}
	if want := string(oauth.XOAUTH2Payload(user, token)); rec.authPayload != want {
		t.Errorf("auth payload:\n got %q\nwant %q", rec.authPayload, want)
	}
}
