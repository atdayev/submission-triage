package imap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
)

type fileOp struct {
	uid    uint32
	folder string
}

type fakeMailbox struct {
	msgs          []rawMessage
	seen          []uint32
	flagged       []uint32
	filed         []fileOp
	ops           []string // ordered log of "seen"/"flag"/"file" for ordering assertions
	fileErr       error
	fetchLimit    int
	fetchErr      error
	closed        bool
	holdIDs       []string // Message-IDs ScanFolder returns
	scannedFolder string   // folder ScanFolder was last asked for
	scanErr       error
}

func (f *fakeMailbox) FetchUnseen(_ context.Context, limit int) ([]rawMessage, error) {
	f.fetchLimit = limit
	return f.msgs, f.fetchErr
}

func (f *fakeMailbox) MarkSeen(_ context.Context, uid uint32) error {
	f.seen = append(f.seen, uid)
	f.ops = append(f.ops, "seen")
	return nil
}

func (f *fakeMailbox) Flag(_ context.Context, uid uint32) error {
	f.flagged = append(f.flagged, uid)
	f.ops = append(f.ops, "flag")
	return nil
}

func (f *fakeMailbox) File(_ context.Context, uid uint32, folder string) error {
	f.ops = append(f.ops, "file")
	if f.fileErr != nil {
		return f.fileErr
	}
	f.filed = append(f.filed, fileOp{uid: uid, folder: folder})
	return nil
}

func (f *fakeMailbox) ScanFolder(_ context.Context, folder string, _ int) ([]string, error) {
	f.scannedFolder = folder
	return f.holdIDs, f.scanErr
}

func (f *fakeMailbox) Close() error {
	f.closed = true
	return nil
}

type fakeIngester struct {
	reqs           []service.IngestRequest
	fn             func(service.IngestRequest) error
	res            *service.IngestResult // returned when set; defaults to awaiting
	fileFailure    *fileOp               // captured AuditFileFailed args
	reconciledHold []string              // Message-IDs passed to ReconcileHold
	reconcileCalls int
}

func (f *fakeIngester) IngestEmail(_ context.Context, req service.IngestRequest) (service.IngestResult, error) {
	f.reqs = append(f.reqs, req)
	if f.fn != nil {
		if err := f.fn(req); err != nil {
			return service.IngestResult{}, err
		}
	}
	if f.res != nil {
		return *f.res, nil
	}
	return service.IngestResult{SubmissionID: "sub-1", State: model.StateAwaiting}, nil
}

func (f *fakeIngester) AuditFileFailed(_ context.Context, _, folder, _ string) {
	f.fileFailure = &fileOp{folder: folder}
}

func (f *fakeIngester) ReconcileHold(_ context.Context, messageIDs []string) error {
	f.reconciledHold = append(f.reconciledHold, messageIDs...)
	f.reconcileCalls++
	return nil
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

func TestPoller_ReconcilesHoldAfterInboxPass(t *testing.T) {
	mb := &fakeMailbox{holdIDs: []string{"m-held"}}
	ing := &fakeIngester{}
	p := testPoller(mb, ing)
	p.holdFolder = "Hold"
	p.holdScanLimit = 200
	p.pollOnce(context.Background())

	if mb.scannedFolder != "Hold" {
		t.Errorf("expected the Hold folder scanned, got %q", mb.scannedFolder)
	}
	if ing.reconcileCalls != 1 {
		t.Fatalf("expected one ReconcileHold call, got %d", ing.reconcileCalls)
	}
	if len(ing.reconciledHold) != 1 || ing.reconciledHold[0] != "m-held" {
		t.Errorf("ReconcileHold got %v, want [m-held]", ing.reconciledHold)
	}
}

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

// filingPoller enables status filing with distinct folder names per state.
func filingPoller(mb mailbox, ing ingester) *Poller {
	p := testPoller(mb, ing)
	p.fileByStatus = true
	p.folders = statusFolders{
		complete:      "Ready for Underwriting",
		awaiting:      "Waiting on Broker",
		escalated:     "Escalated",
		unknownPolicy: "Unknown Policy",
	}
	return p
}

func TestPoller_FilesToStatusFolder(t *testing.T) {
	cases := []struct {
		name   string
		res    service.IngestResult
		folder string
	}{
		{"complete", service.IngestResult{SubmissionID: "s", State: model.StateComplete}, "Ready for Underwriting"},
		{"awaiting", service.IngestResult{SubmissionID: "s", State: model.StateAwaiting}, "Waiting on Broker"},
		{"escalated", service.IngestResult{SubmissionID: "s", State: model.StateEscalated}, "Escalated"},
		{"policy_unknown", service.IngestResult{SubmissionID: "s", State: model.StateAwaiting, PolicyUnknown: true}, "Unknown Policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
			res := tc.res
			filingPoller(mb, &fakeIngester{res: &res}).pollOnce(context.Background())
			if len(mb.filed) != 1 || mb.filed[0] != (fileOp{uid: 7, folder: tc.folder}) {
				t.Fatalf("filed: got %v, want [{7 %q}]", mb.filed, tc.folder)
			}
		})
	}
}

func TestPoller_MarksSeenBeforeFile(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateComplete}}
	filingPoller(mb, ing).pollOnce(context.Background())

	// MOVE removes the source UID, so \Seen must be set first
	if len(mb.ops) != 2 || mb.ops[0] != "seen" || mb.ops[1] != "file" {
		t.Fatalf("op order: got %v, want [seen file]", mb.ops)
	}
}

func TestPoller_FlagsNeedsReviewBetweenSeenAndFile(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateAwaiting, NeedsReview: true}}
	filingPoller(mb, ing).pollOnce(context.Background())

	// order matters: MOVE invalidates the source UID, so \Seen and \Flagged go first
	if len(mb.ops) != 3 || mb.ops[0] != "seen" || mb.ops[1] != "flag" || mb.ops[2] != "file" {
		t.Fatalf("op order: got %v, want [seen flag file]", mb.ops)
	}
	if len(mb.flagged) != 1 || mb.flagged[0] != 7 {
		t.Errorf("flagged: got %v, want [7]", mb.flagged)
	}
	// a flagged awaiting submission still files by state
	if len(mb.filed) != 1 || mb.filed[0] != (fileOp{uid: 7, folder: "Waiting on Broker"}) {
		t.Errorf("filed: got %v", mb.filed)
	}
}

func TestPoller_DoesNotFlagWhenNotNeedsReview(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateComplete}}
	filingPoller(mb, ing).pollOnce(context.Background())

	if len(mb.flagged) != 0 {
		t.Errorf("a non-review message must not be flagged: got %v", mb.flagged)
	}
	if len(mb.ops) != 2 || mb.ops[0] != "seen" || mb.ops[1] != "file" {
		t.Fatalf("op order: got %v, want [seen file]", mb.ops)
	}
}

func TestPoller_FileFailureIsNonFatal(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}, fileErr: errors.New("move refused")}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s-x", State: model.StateComplete}}
	filingPoller(mb, ing).pollOnce(context.Background())

	// still marked seen (won't reprocess); file failure audited, ingest not failed
	if len(mb.seen) != 1 {
		t.Errorf("message should still be marked seen: %v", mb.seen)
	}
	if ing.fileFailure == nil || ing.fileFailure.folder != "Ready for Underwriting" {
		t.Errorf("file failure not audited: %+v", ing.fileFailure)
	}
}

func TestPoller_NoFileOnDuplicate(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateComplete, IsDuplicate: true}}
	filingPoller(mb, ing).pollOnce(context.Background())

	if len(mb.filed) != 0 {
		t.Errorf("a duplicate redelivery should not re-file: got %v", mb.filed)
	}
}

func TestPoller_FileByStatusDisabledFilesNothing(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateComplete}}
	testPoller(mb, ing).pollOnce(context.Background()) // fileByStatus false

	if len(mb.filed) != 0 {
		t.Errorf("IMAP_FILE_BY_STATUS=false must file nothing: got %v", mb.filed)
	}
	if len(mb.seen) != 1 {
		t.Errorf("message should still be marked seen even without filing: %v", mb.seen)
	}
}

func TestPoller_MarkSeenFailureSkipsFile(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 7, Raw: []byte(validEML)}}}
	ing := &fakeIngester{res: &service.IngestResult{SubmissionID: "s", State: model.StateComplete}}
	p := filingPoller(mb, ing)
	// wrap the mailbox so MarkSeen fails
	mbFail := &markSeenFailMailbox{fakeMailbox: mb}
	p.dial = func(context.Context) (mailbox, error) { return mbFail, nil }
	p.pollOnce(context.Background())

	if len(mb.filed) != 0 {
		t.Errorf("a mark-seen failure must skip filing (would move an unread message): %v", mb.filed)
	}
}

// markSeenFailMailbox fails MarkSeen but otherwise delegates.
type markSeenFailMailbox struct{ *fakeMailbox }

func (m *markSeenFailMailbox) MarkSeen(context.Context, uint32) error {
	return errors.New("store failed")
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

func TestNewPoller_SeedsLastPollAtStartup(t *testing.T) {
	before := time.Now().Add(-time.Second)
	p := NewPoller(
		config.IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 30},
		PasswordAuth("u", "p"),
		&fakeIngester{},
		logrus.NewEntry(logrus.New()),
	)
	// a freshly booted poller must not read as stale before its first tick
	if p.LastSuccessfulPoll().Before(before) {
		t.Errorf("NewPoller must seed lastSuccessfulPoll with startup time, got %v", p.LastSuccessfulPoll())
	}
}

func TestPoller_RecordsSuccessOnZeroMessageTick(t *testing.T) {
	p := testPoller(&fakeMailbox{}, &fakeIngester{}) // no messages
	before := time.Now()
	p.pollOnce(context.Background())
	if got := p.LastSuccessfulPoll(); got.Before(before) {
		t.Errorf("a zero-message tick should still record a successful poll: got %v", got)
	}
	if p.failures.Load() != 0 {
		t.Errorf("a successful poll should reset failures, got %d", p.failures.Load())
	}
}

func TestPoller_FailureDoesNotAdvanceLastPoll(t *testing.T) {
	p := testPoller(nil, &fakeIngester{})
	p.dial = func(context.Context) (mailbox, error) { return nil, errors.New("refused") }
	last := p.LastSuccessfulPoll()
	p.pollOnce(context.Background())
	if p.failures.Load() != 1 {
		t.Errorf("dial failure should increment consecutive failures, got %d", p.failures.Load())
	}
	if !p.LastSuccessfulPoll().Equal(last) {
		t.Error("a failed poll must not advance lastSuccessfulPoll")
	}
}
