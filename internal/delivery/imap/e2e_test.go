package imap

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// capturingSender stands in for SMTP and records what the service sends.
type capturingSender struct {
	mu   sync.Mutex
	sent []model.Reply
}

func (c *capturingSender) Name() string { return "capture" }

func (c *capturingSender) SendThreadedReply(_ context.Context, r model.Reply) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, r)
	return "captured", nil
}

func (c *capturingSender) replies() []model.Reply {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.Reply(nil), c.sent...)
}

// TestE2E_RealIMAPTrigger drives the whole product the way production does:
// the real poller fetches the corpus from a live in-memory IMAP server, runs it
// through the real service (real SQLite, heuristic classifier, real checklists),
// and the reply goes out a captured sender. It asserts the inbound trigger
// (fetch + mark-seen), the resulting submission states (incl. follow-up
// threading), the audit trail, the actual reply content, and escalation.
func TestE2E_RealIMAPTrigger(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	ctx := context.Background()
	log := logrus.NewEntry(logrus.New())

	emls := loadCorpus(t, filepath.Join(repoRoot, "testdata", "eml"))
	addr := startMemServer(t, emls...)
	useInsecureDial(t)

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"), log)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db, filepath.Join(repoRoot, "migrations"), log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := repository.NewRepository(db, log)
	clStore, err := checklist.NewYAMLStore(filepath.Join(repoRoot, "checklists"), log)
	if err != nil {
		t.Fatalf("checklists: %v", err)
	}
	pdf, csv := extractor.NewPDF(), extractor.NewCSV()
	sender := &capturingSender{}
	svc := service.NewSubmissionsService(service.Dependencies{
		Config: &config.Config{
			Escalation: config.EscalationConfig{IntervalMinutes: 60, ThresholdHours: 72},
			Retry:      config.RetryConfig{Attempts: 1, BaseDelayMs: 1},
			Reply:      config.ReplyConfig{Workers: 2, QueueSize: 32},
		},
		Repository:     repo,
		EmailSender:    sender,
		Classifier:     classifier.NewHeuristicLLMClassifier(nil),
		ChecklistStore: clStore,
		TextExtractors: map[string]service.TextExtractor{
			"application/pdf":   pdf,
			"application/x-pdf": pdf,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document": extractor.NewDOCX(),
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       extractor.NewXLSX(),
			"text/csv":        csv,
			"application/csv": csv,
		},
		Log: log,
	})
	t.Cleanup(svc.Shutdown)

	// The real trigger: poll the mailbox exactly as the running service does.
	NewPoller(cfgFor(addr), svc, log).pollOnce(ctx)
	svc.Wait() // let the reply workers finish

	// Mark-seen worked: a second fetch finds nothing left to process.
	mb, err := dialIMAP(cfgFor(addr), log)(ctx)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	left, err := mb.FetchUnseen(ctx, 50)
	_ = mb.Close()
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("messages not marked seen: %d still unseen", len(left))
	}

	// Final states: the follow-ups (08-10) must thread onto the submissions
	// 04/05/07 opened and complete them — not create new submissions.
	counts := stateCounts(t, db)
	if counts[model.StateComplete] != 6 {
		t.Errorf("complete: got %d, want 6 (%v)", counts[model.StateComplete], counts)
	}
	if counts[model.StateAwaiting] != 3 {
		t.Errorf("awaiting: got %d, want 3 (%v)", counts[model.StateAwaiting], counts)
	}

	// The product's actual output: one reply per ingested email, each threaded,
	// and the oakview "missing loss runs" case must say so.
	replies := sender.replies()
	if len(replies) != len(emls) {
		t.Errorf("replies sent: got %d, want %d", len(replies), len(emls))
	}
	var sawLossRun bool
	for _, r := range replies {
		if r.ToAddress == "" || !strings.HasPrefix(r.Subject, "Re:") {
			t.Errorf("reply not addressed/threaded: %+v", r)
		}
		if strings.Contains(strings.ToLower(r.BodyText), "loss run") {
			sawLossRun = true
			if r.InReplyTo == "" {
				t.Error("missing-items reply not threaded (empty In-Reply-To)")
			}
		}
	}
	if !sawLossRun {
		t.Error("no reply mentioned the missing loss runs — wrong outbound content")
	}

	// Audit trail recorded the work, including replies actually sent.
	if n := auditCount(t, db, string(model.EventChecklistEvaluated)); n < len(emls) {
		t.Errorf("checklist.evaluated audits: got %d, want >= %d", n, len(emls))
	}
	if n := auditCount(t, db, string(model.EventReplySent)); n < len(emls) {
		t.Errorf("reply.sent audits: got %d, want >= %d", n, len(emls))
	}

	// The freshness clock is driven by processing time (not the sender's Date
	// header), so simulate aged cases by back-dating the awaiting rows well past
	// any threshold, then confirm they escalate.
	aged := time.Now().Add(-10000 * time.Hour).UnixNano()
	if _, err := db.ExecContext(ctx, "UPDATE submissions SET last_action_at = ? WHERE state = ?",
		aged, string(model.StateAwaiting)); err != nil {
		t.Fatalf("age awaiting: %v", err)
	}
	if err := svc.CheckEscalations(ctx); err != nil {
		t.Fatalf("escalations: %v", err)
	}
	esc, err := repo.Submissions.ListEscalatedSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("list escalated: %v", err)
	}
	if len(esc) < 2 {
		t.Errorf("expected >= 2 escalated stale submissions, got %d", len(esc))
	}
}

func loadCorpus(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func stateCounts(t *testing.T, db *sql.DB) map[model.State]int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT state, COUNT(*) FROM submissions GROUP BY state")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[model.State]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[model.State(s)] = n
	}
	return out
}

func auditCount(t *testing.T, db *sql.DB, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM audit_log WHERE event_type = ?", eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
