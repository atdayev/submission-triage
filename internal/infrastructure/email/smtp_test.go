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

func TestSMTP_EncodesNonASCIISubjectAndName(t *testing.T) {
	var captured []byte
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, msg []byte) error {
		captured = msg
		return nil
	})
	s.cfg.FromName = "Café Triage"

	if _, err := s.SendThreadedReply(context.Background(), model.Reply{
		ToAddress: "broker@x",
		Subject:   "Re: Validación de póliza",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	m := string(captured)
	// raw 8-bit bytes must never reach the headers; they must be RFC2047-encoded
	if strings.Contains(m, "Validación") || strings.Contains(m, "Café") {
		t.Errorf("non-ASCII left raw in headers:\n%s", m)
	}
	if !strings.Contains(m, "Subject: =?utf-8?q?") {
		t.Errorf("subject not RFC2047-encoded:\n%s", m)
	}
	if !strings.Contains(m, "From: =?utf-8?q?") {
		t.Errorf("from-name not RFC2047-encoded:\n%s", m)
	}
}

func TestSMTP_NeutralizesHeaderInjection(t *testing.T) {
	var captured []byte
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, msg []byte) error {
		captured = msg
		return nil
	})

	if _, err := s.SendThreadedReply(context.Background(), model.Reply{
		ToAddress: "broker@x\r\nBcc: evil@z",
		Subject:   "ok\r\nX-Injected: yes",
		InReplyTo: "root@x\r\nX-Evil: 1",
		BodyText:  "body",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	m := string(captured)
	// no crafted field may start a new header line in the rendered message
	for _, bad := range []string{"\nBcc:", "\nX-Injected:", "\nX-Evil:"} {
		if strings.Contains(m, bad) {
			t.Errorf("injected header line %q survived:\n%s", strings.TrimPrefix(bad, "\n"), m)
		}
	}
	if n := strings.Count(m, "\r\nTo: "); n != 1 {
		t.Errorf("expected exactly one To header, got %d:\n%s", n, m)
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

func TestSMTP_DeterministicMessageIDAcrossSends(t *testing.T) {
	// a redelivery of the same reply must reuse the Message-ID so a receiver dedupes it
	s := testSMTP(func(_ context.Context, _ config.SMTPConfig, _ string, _ []string, _ []byte) error {
		return nil
	})
	r := model.Reply{SubmissionID: "sub-1", ToAddress: "broker@x", Subject: "Re: thread", BodyText: "need loss runs"}
	id1, err := s.SendThreadedReply(context.Background(), r)
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	id2, err := s.SendThreadedReply(context.Background(), r)
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("Message-ID not stable across redeliveries: %q vs %q", id1, id2)
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

func TestBuildMessage_SanitizesThreadingRefs(t *testing.T) {
	s := testSMTP(nil)
	msg := string(s.buildMessage(model.Reply{
		ToAddress:  "broker@x",
		Subject:    "Re: thread",
		BodyText:   "b",
		InReplyTo:  "  weird id  ",
		References: []string{"root@x", "p r e v@x"},
	}, "mid@host"))

	// internal whitespace/brackets are stripped within each id; ids stay
	// space-separated, so "p r e v@x" becomes "<prev@x>"
	for _, want := range []string{
		"In-Reply-To: <weirdid>\r\n",
		"References: <root@x> <prev@x>\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing sanitized header %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "p r e v") {
		t.Errorf("internal whitespace not stripped from reference:\n%s", msg)
	}
}

func TestBuildMessage_FoldsAndCapsReferences(t *testing.T) {
	refs := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		refs = append(refs, "abcdefghijklmnopqrstuvwxyz0123456789@mail.example.com")
	}
	s := testSMTP(nil)
	msg := string(s.buildMessage(model.Reply{ToAddress: "b@x", Subject: "Re: x", BodyText: "b", References: refs}, "mid@host"))

	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line exceeds RFC 5322 998-octet limit: %d octets", len(line))
		}
	}
	// at most maxRefs tokens survive
	if n := strings.Count(msg, "@mail.example.com"); n > maxRefs {
		t.Errorf("References not capped: %d tokens > %d", n, maxRefs)
	}
}

func TestBuildMessage_TruncatesLongSubject(t *testing.T) {
	s := testSMTP(nil)
	long := strings.Repeat("A", 4000)
	msg := string(s.buildMessage(model.Reply{ToAddress: "b@x", Subject: long, BodyText: "b"}, "mid@host"))
	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("subject produced a header line over 998 octets: %d", len(line))
		}
	}
}

func TestBuildMessage_DropsAbsurdlyLongRef(t *testing.T) {
	s := testSMTP(nil)
	longRef := strings.Repeat("x", 2000) + "@h" // a single oversized message-id
	msg := string(s.buildMessage(model.Reply{
		ToAddress:  "b@x",
		Subject:    "Re: x",
		BodyText:   "b",
		References: []string{"root@x", longRef},
	}, "mid@host"))

	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a single long ref produced a header line over 998 octets: %d", len(line))
		}
	}
	if !strings.Contains(msg, "<root@x>") {
		t.Errorf("legit reference dropped:\n%s", msg)
	}
	if strings.Contains(msg, longRef) {
		t.Errorf("absurd reference not dropped:\n%s", msg)
	}
}
