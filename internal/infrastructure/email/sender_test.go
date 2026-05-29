package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
)

func testSender(t *testing.T, url string) *PostmarkSender {
	t.Helper()
	log := logrus.NewEntry(logrus.New())
	s := NewPostmarkSender(config.PostmarkConfig{
		ServerToken: "test-token",
		FromAddress: "noreply@example.com",
		FromName:    "Triage",
	}, 3, time.Millisecond, log)
	s.endpoint = url
	return s
}

func TestPostmark_Send_HappyPath_HeadersAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Postmark-Server-Token") != "test-token" {
			t.Errorf("missing server token: %v", r.Header)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var req postmarkSendRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if req.From != "Triage <noreply@example.com>" {
			t.Errorf("From: %q", req.From)
		}
		if req.To != "broker@x" {
			t.Errorf("To: %q", req.To)
		}
		if req.Subject != "Re: thread" {
			t.Errorf("Subject: %q", req.Subject)
		}
		hMap := map[string]string{}
		for _, h := range req.Headers {
			hMap[h.Name] = h.Value
		}
		if hMap["In-Reply-To"] != "<root@x>" {
			t.Errorf("In-Reply-To: %q", hMap["In-Reply-To"])
		}
		if hMap["References"] != "<root@x> <prev@x>" {
			t.Errorf("References: %q", hMap["References"])
		}
		json.NewEncoder(w).Encode(postmarkSendResponse{MessageID: "pm-success-id"})
	}))
	defer srv.Close()

	s := testSender(t, srv.URL)
	id, err := s.SendThreadedReply(context.Background(), model.Reply{
		ToAddress:  "broker@x",
		Subject:    "Re: thread",
		BodyText:   "hello",
		InReplyTo:  "<root@x>",
		References: []string{"<root@x>", "<prev@x>"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "pm-success-id" {
		t.Errorf("id: got %q", id)
	}
}

func TestPostmark_429_Retried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(postmarkSendResponse{Message: "rate limited"})
			return
		}
		json.NewEncoder(w).Encode(postmarkSendResponse{MessageID: "ok"})
	}))
	defer srv.Close()

	s := testSender(t, srv.URL)
	id, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y", Subject: "s"})
	if err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if id != "ok" {
		t.Errorf("id: got %q", id)
	}
	if hits.Load() < 2 {
		t.Errorf("expected ≥2 hits, got %d", hits.Load())
	}
}

func TestPostmark_4xx_NotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(postmarkSendResponse{ErrorCode: 405, Message: "bad sender"})
	}))
	defer srv.Close()

	s := testSender(t, srv.URL)
	_, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y", Subject: "s"})
	if err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 hit (permanent), got %d", hits.Load())
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error should mention 422: %v", err)
	}
}

func TestPostmark_EmptyServerToken_FailsLocally(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	s := NewPostmarkSender(config.PostmarkConfig{}, 1, time.Millisecond, log)
	_, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y"})
	if err == nil || !strings.Contains(err.Error(), "server token") {
		t.Errorf("expected server-token error, got %v", err)
	}
}

func TestPostmark_EmptyRecipient_FailsLocally(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	s := NewPostmarkSender(config.PostmarkConfig{ServerToken: "x"}, 1, time.Millisecond, log)
	_, err := s.SendThreadedReply(context.Background(), model.Reply{Subject: "s"})
	if err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Errorf("expected recipient error, got %v", err)
	}
}

func TestLogSender_ReturnsUniqueID(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	s := NewLogSender(log)
	id1, err := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.SendThreadedReply(context.Background(), model.Reply{ToAddress: "x@y"})
	if id1 == "" || id2 == "" {
		t.Error("ids should be non-empty")
	}
	if id1 == id2 {
		t.Errorf("ids should differ: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "log-sender-") {
		t.Errorf("expected log-sender prefix, got %q", id1)
	}
}
