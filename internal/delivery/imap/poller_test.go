package imap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
)

type fakeMailbox struct {
	msgs       []rawMessage
	seen       []uint32
	fetchLimit int
	fetchErr   error
	closed     bool
}

func (f *fakeMailbox) FetchUnseen(_ context.Context, limit int) ([]rawMessage, error) {
	f.fetchLimit = limit
	return f.msgs, f.fetchErr
}

func (f *fakeMailbox) MarkSeen(_ context.Context, uid uint32) error {
	f.seen = append(f.seen, uid)
	return nil
}

func (f *fakeMailbox) Close() error {
	f.closed = true
	return nil
}

type fakeIngester struct {
	reqs []service.IngestRequest
	fn   func(service.IngestRequest) error
}

func (f *fakeIngester) IngestEmail(_ context.Context, req service.IngestRequest) (service.IngestResult, error) {
	f.reqs = append(f.reqs, req)
	if f.fn != nil {
		if err := f.fn(req); err != nil {
			return service.IngestResult{}, err
		}
	}
	return service.IngestResult{SubmissionID: "sub-1", State: model.StateAwaiting}, nil
}

func testPoller(mb mailbox, ing ingester) *Poller {
	return &Poller{
		dial:       func(context.Context) (mailbox, error) { return mb, nil },
		ingest:     ing,
		interval:   time.Hour,
		batchLimit: 50,
		mailbox:    "INBOX",
		log:        logrus.NewEntry(logrus.New()),
	}
}

const validEML = "From: Alice <alice@example.com>\r\n" +
	"To: ops@agency.example\r\n" +
	"Subject: New Submission - CGL\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"Date: Mon, 19 May 2026 09:00:00 -0400\r\n" +
	"\r\n" +
	"Please find the application attached.\r\n"

func TestPoller_IngestsAndMarksSeen(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{}
	testPoller(mb, ing).pollOnce(context.Background())

	if len(ing.reqs) != 1 {
		t.Fatalf("ingest calls: got %d, want 1", len(ing.reqs))
	}
	if ing.reqs[0].Source != "imap" {
		t.Errorf("source: got %q, want imap", ing.reqs[0].Source)
	}
	if ing.reqs[0].MessageID != "m1@example.com" {
		t.Errorf("message id: got %q", ing.reqs[0].MessageID)
	}
	if len(mb.seen) != 1 || mb.seen[0] != 7 {
		t.Errorf("marked seen: got %v, want [7]", mb.seen)
	}
	if !mb.closed {
		t.Error("mailbox not closed")
	}
	if mb.fetchLimit != 50 {
		t.Errorf("fetch limit: got %d, want 50", mb.fetchLimit)
	}
}

func TestPoller_LeavesUnreadOnIngestFailure(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 9, Raw: []byte(validEML)}}}
	ing := &fakeIngester{fn: func(service.IngestRequest) error { return errors.New("db down") }}
	testPoller(mb, ing).pollOnce(context.Background())

	if len(ing.reqs) != 1 {
		t.Fatalf("ingest calls: got %d, want 1", len(ing.reqs))
	}
	if len(mb.seen) != 0 {
		t.Errorf("should not mark seen on ingest failure: got %v", mb.seen)
	}
}

func TestPoller_MarksUnparseableSeenToSkip(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 3, Raw: []byte("this is not an email")}}}
	ing := &fakeIngester{}
	testPoller(mb, ing).pollOnce(context.Background())

	if len(ing.reqs) != 0 {
		t.Errorf("unparseable message should not be ingested: got %d", len(ing.reqs))
	}
	if len(mb.seen) != 1 || mb.seen[0] != 3 {
		t.Errorf("poison message should be marked seen: got %v", mb.seen)
	}
}

func TestPoller_DialFailureIsGraceful(t *testing.T) {
	ing := &fakeIngester{}
	p := testPoller(nil, ing)
	p.dial = func(context.Context) (mailbox, error) { return nil, errors.New("connect refused") }
	p.pollOnce(context.Background()) // must not panic
	if len(ing.reqs) != 0 {
		t.Errorf("no ingest expected on dial failure: got %d", len(ing.reqs))
	}
}

func TestPoller_FetchErrorStillClosesMailbox(t *testing.T) {
	mb := &fakeMailbox{fetchErr: errors.New("search failed")}
	testPoller(mb, &fakeIngester{}).pollOnce(context.Background())
	if !mb.closed {
		t.Error("mailbox should be closed even when fetch fails")
	}
}
