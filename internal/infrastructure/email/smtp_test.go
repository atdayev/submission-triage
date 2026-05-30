package email

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/retry"
)

func testSMTP(send smtpSendFn) *SMTPSender {
	return &SMTPSender{
		cfg: config.SMTPConfig{
			Host:        "smtp.example.com",
			Port:        "587",
			Username:    "ops@agency.example",
			Password:    "app-password",
			FromAddress: "ops@agency.example",
			FromName:    "Triage",
		},
		send:          send,
		log:           logrus.NewEntry(logrus.New()),
		retryAttempts: 3,
		retryBase:     time.Millisecond,
	}
}

func TestSMTP_HappyPath_HeadersAndThreading(t *testing.T) {
	var captured []byte
	var gotFrom string
	var gotTo []string
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, from string, to []string, msg []byte) error {
		captured, gotFrom, gotTo = msg, from, to
		return nil
	})

	id, err := s.SendThreadedReply(context.Background(), model.Reply{
		ToAddress:  "broker@x",
		Subject:    "Re: thread",
		BodyText:   "still need the loss runs",
		InReplyTo:  "root@x",
		References: []string{"root@x", "prev@x"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.HasSuffix(id, "@agency.example") {
		t.Errorf("message id domain: got %q", id)
	}
	if gotFrom != "ops@agency.example" {
		t.Errorf("envelope from: got %q", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "broker@x" {
		t.Errorf("envelope to: got %v", gotTo)
	}

	m := string(captured)
	for _, want := range []string{
		"From: Triage <ops@agency.example>\r\n",
		"To: broker@x\r\n",
		"Subject: Re: thread\r\n",
		"Message-ID: <" + id + ">\r\n",
		"In-Reply-To: <root@x>\r\n",
		"References: <root@x> <prev@x>\r\n",
		"\r\nstill need the loss runs",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message missing %q\n---\n%s", want, m)
		}
	}
}

func TestSMTP_OmitsThreadingHeadersWhenAbsent(t *testing.T) {
	var captured []byte
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, msg []byte) error {
		captured = msg
		return nil
	})
	if _, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@x", Subject: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(string(captured), "In-Reply-To:") || strings.Contains(string(captured), "References:") {
		t.Errorf("unexpected threading headers:\n%s", captured)
	}
}

func TestSMTP_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, _ []byte) error {
		if calls.Add(1) == 1 {
			return errors.New("connection reset")
		}
		return nil
	})
	if _, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("send calls: got %d, want 2", got)
	}
}

func TestSMTP_PermanentErrorNotRetried(t *testing.T) {
	var calls atomic.Int32
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, _ []byte) error {
		calls.Add(1)
		return retry.MarkPermanent(errors.New("535 auth failed"))
	})
	if _, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@x"}); err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("send calls: got %d, want 1 (permanent)", got)
	}
}

func TestSMTP_ReuseSameMessageIDAcrossRetries(t *testing.T) {
	var ids []string
	var calls atomic.Int32
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, msg []byte) error {
		// pull the Message-ID line out of each attempt's message
		for _, line := range strings.Split(string(msg), "\r\n") {
			if strings.HasPrefix(line, "Message-ID: ") {
				ids = append(ids, line)
			}
		}
		if calls.Add(1) == 1 {
			return errors.New("temporary failure")
		}
		return nil
	})
	if _, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Errorf("Message-ID changed across retries: %v", ids)
	}
}

func TestSMTP_ValidationErrors(t *testing.T) {
	called := false
	send := func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, _ []byte) error {
		called = true
		return nil
	}

	noRecipient := testSMTP(send)
	if _, err := noRecipient.SendThreadedReply(context.Background(), model.Reply{Subject: "x"}); err == nil {
		t.Error("expected error for empty recipient")
	}

	noHost := testSMTP(send)
	noHost.cfg.Host = ""
	if _, err := noHost.SendThreadedReply(context.Background(), model.Reply{ToAddress: "broker@x"}); err == nil {
		t.Error("expected error for empty host")
	}

	if called {
		t.Error("send should not be called when validation fails")
	}
}

func TestSMTP_Name(t *testing.T) {
	if got := testSMTP(nil).Name(); got != "smtp" {
		t.Errorf("Name: got %q, want smtp", got)
	}
}
