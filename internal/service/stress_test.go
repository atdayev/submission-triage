//go:build stress

// Stress tests: the service against a real SQLite file with real concurrency, so write
// contention, dedup, the one-pending-per-submission index and the sweep caps are the
// real implementations rather than fakes.
//
//	go test -tags stress ./internal/service/ -v
package service

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
)

const (
	stressConcurrency = 32   // goroutines hammering one entry point
	stressVolume      = 200  // submissions in a volume run — one agency-day
	stressAuditRows   = 5000 // audit rows for the retention sweep
)

// A1 — the same message delivered many times at once must yield one submission.
// singleflight collapses the in-process race; the deterministic-id dedup catches
// anything that slips past it.
func TestStress_ConcurrentDuplicateDelivery(t *testing.T) {
	h := newRealHarness(t, realCfg())
	req := submissionReq("dup-1@example.com", "New Submission - CGL")

	var wg sync.WaitGroup
	errs := make(chan error, stressConcurrency)
	for i := 0; i < stressConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.svc.IngestEmail(context.Background(), req); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ingest: %v", err)
	}
	h.svc.Wait()

	if n := h.count(`SELECT COUNT(*) FROM submissions`); n != 1 {
		t.Errorf("%d submissions from %d deliveries of one message, want 1", n, stressConcurrency)
	}
	if n := h.count(`SELECT COUNT(*) FROM emails WHERE direction = 'inbound'`); n != 1 {
		t.Errorf("%d inbound emails, want 1", n)
	}
	if n := h.mail.count(); n != 1 {
		t.Errorf("%d replies sent, want 1 — a duplicate delivery must not re-ask the broker", n)
	}
}

// A2 — distinct submissions ingested concurrently. SQLite allows one writer at a
// time regardless of the pool, so this is where write contention shows up: every
// submission must still land, and every one must get its reply.
func TestStress_ConcurrentDistinctSubmissions(t *testing.T) {
	h := newRealHarness(t, realCfg())

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, stressVolume)
	sem := make(chan struct{}, stressConcurrency)
	for i := 0; i < stressVolume; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := submissionReq(fmt.Sprintf("vol-%d@example.com", i), "New Submission - CGL")
			if _, err := h.svc.IngestEmail(context.Background(), req); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		if failed <= 3 {
			t.Errorf("ingest failed under contention: %v", err)
		}
	}
	h.svc.Wait()
	elapsed := time.Since(start)

	if n := h.count(`SELECT COUNT(*) FROM submissions`); n != stressVolume {
		t.Errorf("%d submissions persisted, want %d — writes were lost under contention", n, stressVolume)
	}
	if n := h.count(`SELECT COUNT(*) FROM outbox`); n != stressVolume {
		t.Errorf("%d outbox rows, want %d", n, stressVolume)
	}
	online := h.mail.count()
	t.Logf("%d submissions in %v (%.0f/s); %d sent online, %d overflowed to the sweeper, %d failed",
		stressVolume, elapsed.Round(time.Millisecond),
		float64(stressVolume)/elapsed.Seconds(), online, stressVolume-online, failed)

	// the pool is bounded (REPLY_WORKERS over REPLY_QUEUE_SIZE), so a burst this size
	// overflows by design — durability means the sweeper still delivers every one
	for i := 0; i < 5 && h.count(`SELECT COUNT(*) FROM outbox WHERE status = 'pending'`) > 0; i++ {
		if err := h.svc.RedeliverOutbox(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}
	if stuck := h.count(`SELECT COUNT(*) FROM outbox WHERE status = 'pending'`); stuck != 0 {
		t.Errorf("%d replies still undelivered after 5 sweeps", stuck)
	}
	for id, n := range h.mail.countBySubmission() {
		if n > 1 {
			t.Errorf("submission %s received %d replies across the online + sweeper paths, want 1", id, n)
		}
	}
}

// A3 — a burst of follow-ups on ONE submission. The outbox's partial unique index
// allows a single pending row per submission, so rapid follow-ups must coalesce
// rather than queueing a reply each.
func TestStress_ReplyCoalescingUnderBurst(t *testing.T) {
	cfg := realCfg()
	cfg.Reply.CoalesceWindowSeconds = 3600 // hold everything after the first
	h := newRealHarness(t, cfg)

	if _, err := h.svc.IngestEmail(context.Background(),
		submissionReq("burst-root@example.com", "New Submission - CGL")); err != nil {
		t.Fatalf("root ingest: %v", err)
	}
	h.svc.Wait()

	// follow-ups that each change the outstanding set, so the fingerprint gate lets
	// them through and the coalescing is what has to hold the line
	var wg sync.WaitGroup
	for i := 0; i < stressConcurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = h.svc.IngestEmail(context.Background(), IngestRequest{
				MessageID: fmt.Sprintf("burst-%d@example.com", i), InReplyTo: "burst-root@example.com",
				FromAddress: "broker@example.com", Subject: "Re: New Submission - CGL",
				BodyText: fmt.Sprintf("follow-up %d", i),
			})
		}(i)
	}
	wg.Wait()
	h.svc.Wait()

	pending := h.count(`SELECT COUNT(*) FROM outbox WHERE status = 'pending'`)
	if pending > 1 {
		t.Errorf("%d pending outbox rows for one submission, want at most 1 — coalescing failed", pending)
	}
	byID := h.mail.countBySubmission()
	for id, n := range byID {
		if n > 1 {
			t.Errorf("submission %s received %d replies during the burst, want 1", id, n)
		}
	}
}

// A4 — the online dispatcher and the outbox sweeper both want the same due rows.
// The in-memory `dispatching` set plus the version-guarded mark-sent are what stop
// a row going out twice; this drives both at once.
func TestStress_OnlineDispatchRacesSweeper(t *testing.T) {
	cfg := realCfg()
	h := newRealHarness(t, cfg)
	h.mail.delay = 2 * time.Millisecond // widen the send-then-mark-sent window

	const n = 50
	for i := 0; i < n; i++ {
		if _, err := h.svc.IngestEmail(context.Background(),
			submissionReq(fmt.Sprintf("race-%d@example.com", i), "New Submission - CGL")); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// sweep concurrently with the online pool still draining
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = h.svc.RedeliverOutbox(context.Background())
			}
		}()
	}
	wg.Wait()
	h.svc.Wait()
	_ = h.svc.RedeliverOutbox(context.Background())

	dupes := 0
	for id, sent := range h.mail.countBySubmission() {
		if sent > 1 {
			dupes++
			if dupes <= 3 {
				t.Logf("submission %s received %d replies", id, sent)
			}
		}
	}
	if dupes > 0 {
		t.Errorf("%d submissions received a duplicate reply while the sweeper raced the online pool", dupes)
	}
	if stuck := h.count(`SELECT COUNT(*) FROM outbox WHERE status = 'pending'`); stuck != 0 {
		t.Errorf("%d rows still pending after the sweep drained; replies would be resent", stuck)
	}
}

// A5 — the sweeps are capped per tick with no continuation, so a backlog drains one
// batch per interval. This measures that rather than assuming it.
func TestStress_SweepBatchCapOnLargeBacklog(t *testing.T) {
	h := newRealHarness(t, realCfg())
	ctx := context.Background()
	now := time.Now().UTC()
	stale := now.Add(-200 * 24 * time.Hour)

	const backlog = 250
	for i := 0; i < backlog; i++ {
		if err := h.repo.Submissions.UpsertSubmission(ctx, &model.Submission{
			ID: fmt.Sprintf("stale-%d", i), PolicyType: "cgl", State: model.StateAwaiting,
			CreatedAt: stale, UpdatedAt: stale, LastActionAt: stale,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	if err := h.svc.CheckClosures(ctx); err != nil {
		t.Fatalf("closures: %v", err)
	}
	closed := h.count(`SELECT COUNT(*) FROM submissions WHERE state = 'closed'`)
	if closed == 0 {
		t.Fatal("the stalled sweep closed nothing; awaiting has no other route out")
	}
	if closed > sweepBatch {
		t.Errorf("one tick closed %d rows, above the %d cap", closed, sweepBatch)
	}
	t.Logf("backlog %d: one tick closed %d (cap %d) — %d ticks to drain",
		backlog, closed, sweepBatch, (backlog+sweepBatch-1)/sweepBatch)

	// draining takes repeated ticks; confirm it terminates rather than stalling
	for i := 0; i < 10 && h.count(`SELECT COUNT(*) FROM submissions WHERE state != 'closed'`) > 0; i++ {
		if err := h.svc.CheckClosures(ctx); err != nil {
			t.Fatalf("closures tick %d: %v", i, err)
		}
	}
	if left := h.count(`SELECT COUNT(*) FROM submissions WHERE state != 'closed'`); left != 0 {
		t.Errorf("%d submissions still open after 10 ticks", left)
	}
}

// A6 — the retention sweep against a table big enough to matter. Nothing else in the
// system deletes anything, so this is the only thing between the deploy and a disk
// that fills.
func TestStress_RetentionSweepAtVolume(t *testing.T) {
	h := newRealHarness(t, realCfg())
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-200 * 24 * time.Hour)

	if err := h.repo.Submissions.UpsertSubmission(ctx, &model.Submission{
		ID: "s1", PolicyType: "cgl", State: model.StateClosed,
		CreatedAt: old, UpdatedAt: old, LastActionAt: old,
		Documents: []model.Document{{
			ID: "d1", SubmissionID: "s1", Filename: "big.pdf",
			ExtractedText: string(make([]byte, maxExtractedTextBytes)), CreatedAt: old,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < stressAuditRows; i++ {
		stamp := old
		if i%2 == 0 {
			stamp = now // half are inside the horizon and must survive
		}
		if err := h.repo.Audit.Append(ctx, &model.AuditEntry{
			SubmissionID: "s1", EventType: model.EventEmailReceived, CreatedAt: stamp,
		}); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	touched, err := h.svc.PruneRetention(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	elapsed := time.Since(start)

	surviving := h.count(`SELECT COUNT(*) FROM audit_log`)
	if surviving != stressAuditRows/2 {
		t.Errorf("%d audit rows survived, want %d — the horizon is wrong in one direction", surviving, stressAuditRows/2)
	}
	var text string
	if err := h.dbQueryRow(`SELECT extracted_text FROM documents WHERE id = 'd1'`, &text); err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Error("a long-closed submission's extracted text should be dropped; it is the bulk of its footprint")
	}
	t.Logf("pruned %d rows of %d in %v", touched, stressAuditRows, elapsed.Round(time.Millisecond))
}

// A7 — a submission carrying many attachments. Bounds the per-submission footprint
// and proves the classifier/extractor loop doesn't degrade non-linearly.
func TestStress_ManyAttachmentsOneMessage(t *testing.T) {
	h := newRealHarness(t, realCfg())

	const (
		attachments = 100       // emlparse's maxAttachments
		attachBytes = 300 << 10 // each above maxExtractedTextBytes, so truncation is exercised
	)
	// printable filler, not zero bytes: SQLite's LENGTH() on TEXT stops at the first
	// NUL, so a zero-filled fixture would measure 0 and prove nothing
	filler := bytes.Repeat([]byte("A"), attachBytes)
	atts := make([]model.Attachment, 0, attachments)
	for i := 0; i < attachments; i++ {
		atts = append(atts, model.Attachment{
			Filename:    fmt.Sprintf("ACORD_125_%d.pdf", i),
			ContentType: "application/pdf",
			Content:     filler,
		})
	}

	start := time.Now()
	if _, err := h.svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "fat@example.com", FromAddress: "broker@example.com",
		Subject: "New Submission - CGL", Attachments: atts,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	h.svc.Wait()
	elapsed := time.Since(start)

	if n := h.count(`SELECT COUNT(*) FROM documents`); n != attachments {
		t.Errorf("%d documents stored, want %d", n, attachments)
	}
	// raw attachment bytes must never be persisted — only extracted text is
	var stored int
	if err := h.dbQueryRow(`SELECT COALESCE(SUM(LENGTH(extracted_text)), 0) FROM documents`, &stored); err != nil {
		t.Fatal(err)
	}
	perDoc := stored / attachments
	if perDoc > maxExtractedTextBytes {
		t.Errorf("%d bytes of text per document, above the %d cap", perDoc, maxExtractedTextBytes)
	}
	if perDoc != maxExtractedTextBytes {
		t.Errorf("per-document text %d bytes, want exactly the %d cap — oversized input must truncate, not pass through",
			perDoc, maxExtractedTextBytes)
	}
	rawIn := attachments * attachBytes
	t.Logf("%d attachments (%d MB raw in) in %v; %d MB of extracted text stored — %.0f%% of input",
		attachments, rawIn>>20, elapsed.Round(time.Millisecond), stored>>20, 100*float64(stored)/float64(rawIn))
}

// A8 — the shutdown drain under load. Every queued reply is durable in the outbox
// before dispatch, so shutdown may delay a reply but must never drop one, and it
// must terminate even with a slow sender.
func TestStress_ShutdownDrainsUnderLoad(t *testing.T) {
	h := newRealHarness(t, realCfg())
	h.mail.delay = 5 * time.Millisecond

	const n = 40
	for i := 0; i < n; i++ {
		if _, err := h.svc.IngestEmail(context.Background(),
			submissionReq(fmt.Sprintf("drain-%d@example.com", i), "New Submission - CGL")); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	done := make(chan struct{})
	start := time.Now()
	go func() { h.svc.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not terminate; every wait is supposed to be bounded")
	}
	t.Logf("shutdown drained %d in-flight replies in %v", n, time.Since(start).Round(time.Millisecond))

	// durability: nothing was lost, whether it was sent or left for the sweeper
	rows := h.count(`SELECT COUNT(*) FROM outbox`)
	if rows != n {
		t.Errorf("%d outbox rows survived shutdown, want %d — a queued reply was dropped", rows, n)
	}
}
