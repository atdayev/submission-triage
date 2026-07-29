//go:build integration

// End-to-end tests over the testdata/eml corpus, driven through the real Poller: raw
// .eml bytes → emlparse → emailingest → the real service, classifier, shipped checklists
// and a migrated SQLite database. Every other suite stubs at least one of those.
//
//	go test -tags integration ./internal/delivery/imap/ -run TestE2E -v
package imap

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/database"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/extractor"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/internal/service"
)

const corpusDir = "../../../testdata/eml"

// recordingSender captures replies instead of sending them, minting a unique
// provider Message-ID the way a real SMTP server does.
type recordingSender struct {
	mu   sync.Mutex
	sent []model.Reply
}

func (s *recordingSender) Name() string { return "e2e" }

func (s *recordingSender) SendThreadedReply(_ context.Context, r model.Reply) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, r)
	return "e2e-" + r.SubmissionID + "-" + string(rune('a'+len(s.sent)%26)) + "@test", nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// e2eEnv is the whole pipeline over a real database.
type e2eEnv struct {
	t      *testing.T
	poller *Poller
	svc    *service.SubmissionsService
	mail   *recordingSender
	db     *sql.DB
}

func (e *e2eEnv) count(query string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func newE2E(t *testing.T) *e2eEnv {
	t.Helper()
	log := logrus.NewEntry(logrus.New())
	log.Logger.SetLevel(logrus.ErrorLevel)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "e2e.db"), log)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(context.Background(), db, filepath.Join("..", "..", "..", "migrations"), log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// the shipped checklists, not a fixture: the corpus is graded the way production grades
	store, err := checklist.NewYAMLStore(filepath.Join("..", "..", "..", "checklists"), log)
	if err != nil {
		t.Fatalf("load checklists: %v", err)
	}

	mail := &recordingSender{}
	pdf := extractor.NewPDF()
	svc := service.NewSubmissionsService(service.Dependencies{
		Config: &config.Config{
			Escalation: config.EscalationConfig{
				ThresholdHours: 72, AutoCloseAfterHours: 336, AwaitingAutoCloseAfterHours: 2160,
				BindWindowDays: 7, BindGraceDays: 3, MinGapHours: 24,
			},
			Classifier: config.ClassifierConfig{ConfidenceFloor: 0.80},
			Document:   config.DocumentConfig{UnreadableMinSizeBytes: 51200, UnreadableMinChars: 100},
		},
		Repository:     repository.NewRepository(db, log),
		EmailSender:    mail,
		Classifier:     classifier.NewHeuristicLLMClassifier(nil), // no LLM: heuristics only
		ChecklistStore: store,
		TextExtractors: map[string]service.TextExtractor{"application/pdf": pdf, "application/x-pdf": pdf},
		Log:            log,
	})
	t.Cleanup(svc.Shutdown)

	return &e2eEnv{t: t, svc: svc, mail: mail, db: db, poller: &Poller{
		ingest: svc, control: newFakeControl(), interval: time.Hour,
		batchLimit: 50, maxAttempts: 5, mailbox: "INBOX", log: log,
	}}
}

// deliver feeds the named corpus files to the poller as one mailbox fetch, exactly
// as an IMAP tick would.
func (e *e2eEnv) deliver(files ...string) {
	e.t.Helper()
	msgs := make([]rawMessage, 0, len(files))
	for i, name := range files {
		raw, err := os.ReadFile(filepath.Join(corpusDir, name))
		if err != nil {
			e.t.Fatalf("read %s: %v", name, err)
		}
		msgs = append(msgs, rawMessage{UID: uint32(i + 1), Raw: raw})
	}
	mb := &fakeMailbox{msgs: msgs}
	e.poller.dial = func(context.Context) (mailbox, error) { return mb, nil }
	e.poller.pollOnce(context.Background())
	e.svc.Wait()
}

func corpusFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus glob: %v (%d files)", err, len(paths))
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

// The corpus README documents an expected outcome per file; this is that table.
func TestE2E_CorpusReachesDocumentedState(t *testing.T) {
	cases := []struct {
		file      string
		wantState model.State
		wantReply bool
	}{
		{"01_acme_cgl_complete.eml", model.StateComplete, true},
		{"02_riverbend_bop_complete.eml", model.StateComplete, true},
		{"03_pinegate_wc_complete.eml", model.StateComplete, true},
		{"04_oakview_cgl_missing_lossrun.eml", model.StateAwaiting, true},
		{"05_bluefin_property_missing_two.eml", model.StateAwaiting, true},
		{"06_nimbus_cyber_missing_three.eml", model.StateAwaiting, true},
		{"07_redhill_wc_missing_one.eml", model.StateAwaiting, true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			e := newE2E(t)
			e.deliver(tc.file)

			var state string
			if err := e.db.QueryRow(`SELECT state FROM submissions`).Scan(&state); err != nil {
				t.Fatalf("no submission was created: %v", err)
			}
			if model.State(state) != tc.wantState {
				var missing string
				_ = e.db.QueryRow(`SELECT missing_items FROM submissions`).Scan(&missing)
				t.Errorf("state: got %s, want %s (missing: %s)", state, tc.wantState, missing)
			}
			if got := e.mail.count() > 0; got != tc.wantReply {
				t.Errorf("reply sent = %v, want %v", got, tc.wantReply)
			}
			// the subject names a line of business in every corpus file
			var policy string
			_ = e.db.QueryRow(`SELECT policy_type FROM submissions`).Scan(&policy)
			if policy == model.PolicyTypeUnknown {
				t.Error("policy type did not resolve from the subject")
			}
		})
	}
}

// A follow-up carrying the outstanding document must re-thread onto the original
// submission — not create a second — and complete it.
func TestE2E_FollowUpRethreadsAndCompletes(t *testing.T) {
	cases := []struct{ name, initial, followUp string }{
		{"oakview", "04_oakview_cgl_missing_lossrun.eml", "08_oakview_followup_lossrun.eml"},
		{"bluefin", "05_bluefin_property_missing_two.eml", "09_bluefin_followup_remainder.eml"},
		{"redhill", "07_redhill_wc_missing_one.eml", "10_redhill_followup_payroll.eml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newE2E(t)
			e.deliver(tc.initial)
			if n := e.count(`SELECT COUNT(*) FROM submissions WHERE state = 'awaiting'`); n != 1 {
				t.Fatalf("expected one awaiting submission after the initial mail, got %d", n)
			}

			e.deliver(tc.followUp)
			if n := e.count(`SELECT COUNT(*) FROM submissions`); n != 1 {
				t.Errorf("%d submissions, want 1 — the follow-up split the thread instead of re-threading", n)
			}
			if n := e.count(`SELECT COUNT(*) FROM submissions WHERE state = 'complete'`); n != 1 {
				var missing string
				_ = e.db.QueryRow(`SELECT missing_items FROM submissions`).Scan(&missing)
				t.Errorf("the follow-up should complete the submission; still missing %s", missing)
			}
			// the missing-items ask, then the completion note: two distinct asks
			if n := e.mail.count(); n != 2 {
				t.Errorf("%d replies over the thread, want 2 (missing list, then completion)", n)
			}
		})
	}
}

// The two "stale" corpus files carry Date headers months in the past, and must NOT
// escalate on the tick after ingest: the clock is LastActionAt, stamped when we read the
// message, not the message's own date. testdata/README.md used to claim otherwise.
func TestE2E_StaleDatedMailStartsItsClockAtIngest(t *testing.T) {
	e := newE2E(t)
	e.deliver("11_stalecase_zenith_cgl.eml", "12_stalecase_brookline_bop.eml")
	if n := e.count(`SELECT COUNT(*) FROM submissions WHERE state = 'awaiting'`); n != 2 {
		t.Fatalf("%d awaiting, want 2", n)
	}

	if err := e.svc.CheckEscalations(context.Background()); err != nil {
		t.Fatalf("escalations: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM submissions WHERE state = 'escalated'`); n != 0 {
		t.Errorf("%d escalated immediately after ingest, want 0 — the clock starts when we read the mail", n)
	}

	// the message's own date is still recorded, just not as the escalation clock
	var received, lastAction int64
	if err := e.db.QueryRow(
		`SELECT e.received_at, s.last_action_at FROM emails e
		 JOIN submissions s ON s.id = e.submission_id LIMIT 1`).Scan(&received, &lastAction); err != nil {
		t.Fatal(err)
	}
	if received >= lastAction {
		t.Error("the Date header should be preserved on the email and precede the ingest-time action clock")
	}
	// escalation is a state change and a digest entry, never an email to the broker
	if n := e.mail.count(); n != 2 {
		t.Errorf("%d replies, want 2 (the initial ask for each); escalation must not mail the broker", n)
	}
}

// The mailbox is the retry queue, so the poller re-presents unseen messages after
// any failure. A full redelivery must change nothing and must not write to brokers.
func TestE2E_WholeCorpusIsIdempotent(t *testing.T) {
	e := newE2E(t)
	files := corpusFiles(t)

	e.deliver(files...)
	subs := e.count(`SELECT COUNT(*) FROM submissions`)
	emails := e.count(`SELECT COUNT(*) FROM emails`)
	docs := e.count(`SELECT COUNT(*) FROM documents`)
	replies := e.mail.count()
	if subs == 0 || docs == 0 {
		t.Fatalf("corpus produced nothing: %d submissions, %d documents", subs, docs)
	}

	e.deliver(files...)
	if got := e.count(`SELECT COUNT(*) FROM submissions`); got != subs {
		t.Errorf("submissions after redelivery: got %d, want %d", got, subs)
	}
	if got := e.count(`SELECT COUNT(*) FROM emails`); got != emails {
		t.Errorf("emails after redelivery: got %d, want %d", got, emails)
	}
	if got := e.count(`SELECT COUNT(*) FROM documents`); got != docs {
		t.Errorf("documents after redelivery: got %d, want %d — a redelivery re-stored its attachments", got, docs)
	}
	if got := e.mail.count(); got != replies {
		t.Errorf("replies after redelivery: got %d, want %d — the broker was written to twice", got, replies)
	}
	t.Logf("%d files → %d submissions, %d emails, %d documents, %d replies; stable across redelivery",
		len(files), subs, emails, docs, replies)
}
