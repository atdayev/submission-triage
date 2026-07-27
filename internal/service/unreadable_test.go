package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/extractor"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// scannedPDF builds a valid single-page PDF whose only content is an image (no
// text operators), padded via the image stream so the real extractor reads it
// as empty text — a genuine image-only scan.
func scannedPDF(imageBytes int) []byte {
	var buf bytes.Buffer
	offsets := make([]int, 6)
	writeObj := func(n int, body string) {
		offsets[n] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	buf.WriteString("%PDF-1.4\n")
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
		"/Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>")

	img := bytes.Repeat([]byte{0xAA}, imageBytes)
	offsets[4] = buf.Len()
	fmt.Fprintf(&buf, "4 0 obj\n<< /Type /XObject /Subtype /Image /Width 64 /Height 64 "+
		"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Length %d >>\nstream\n", len(img))
	buf.Write(img)
	buf.WriteString("\nendstream\nendobj\n")

	content := "q 612 0 0 792 0 0 cm /Im0 Do Q"
	offsets[5] = buf.Len()
	fmt.Fprintf(&buf, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)

	xrefOffset := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for n := 1; n <= 5; n++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[n])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", xrefOffset)
	return buf.Bytes()
}

// newSvcRealPDF builds a service wired to the real PDF extractor.
func newSvcRealPDF(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, mail *fakeMail, store checklist.Store) *SubmissionsService {
	t.Helper()
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	cl := store.All()[0]
	return NewSubmissionsService(Dependencies{
		Config: &config.Config{
			Classifier: config.ClassifierConfig{ConfidenceFloor: 0.80},
			Document:   config.DocumentConfig{UnreadableMinSizeBytes: 51200, UnreadableMinChars: 100},
			Escalation: config.EscalationConfig{ThresholdHours: 72},
		},
		Repository:     &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: store,
		TextExtractors: map[string]TextExtractor{"application/pdf": extractor.NewPDF()},
		Log:            logrus.NewEntry(logrus.New()),
	})
}

func TestIngestEmail_ScannedLossRuns_FlaggedNotAskedToResend(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cglChecklistWithLossRuns()}}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	var captured *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	unreadableAudited := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventDocumentUnreadable {
			unreadableAudited = true
		}
	})

	svc := newSvcRealPDF(t, subs, aud, mail, store)

	scanned := scannedPDF(60 * 1024)
	// sanity: the real extractor really does read this as (near-)empty
	if got := svc.extractText(model.Attachment{ContentType: "application/pdf", Content: scanned}); len(strings.TrimSpace(got)) >= 100 {
		t.Fatalf("fixture is not a text-empty scan; extracted %d chars", len(strings.TrimSpace(got)))
	}

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "scanned-lr",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "loss_runs_scan.pdf", ContentType: "application/pdf", Size: len(scanned), Content: scanned},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// an unreadable required doc must not complete or silently soft-pass the field rule
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting", res.State)
	}
	if !res.NeedsReview {
		t.Error("an unreadable scan must flag the submission for review")
	}
	var lossRuns *model.MissingItem
	for i := range res.MissingItems {
		if res.MissingItems[i].ID == "loss_runs" {
			lossRuns = &res.MissingItems[i]
		}
	}
	if lossRuns == nil || lossRuns.Code != model.ReasonUnreadable {
		t.Fatalf("loss_runs should be missing as unreadable, got %+v", res.MissingItems)
	}
	if !unreadableAudited {
		t.Error("expected document.unreadable audit entry")
	}
	// the persisted document carries the unreadable flag; the submission is flagged
	if captured != nil {
		if !captured.NeedsReview {
			t.Error("submission should persist flagged for review")
		}
		found := false
		for _, d := range captured.Documents {
			if d.ClassifiedAs == "loss_runs" && d.Unreadable {
				found = true
			}
		}
		if !found {
			t.Error("loss_runs document should be persisted as unreadable")
		}
	}
	svc.Wait()
	// the reply asks only for the genuinely-absent ACORD 125; the scan is agency-side
	if mail.sentCount() != 1 {
		t.Fatalf("expected the absent item to be requested, got %d sends", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if !contains(body, "ACORD 125") {
		t.Errorf("reply should ask for the absent ACORD 125:\n%s", body)
	}
	if contains(body, "resend") || contains(body, "loss") {
		t.Errorf("the broker must not be told about the unreadable scan:\n%s", body)
	}
}

func TestNormalizeContentType(t *testing.T) {
	cases := map[string]string{
		"text/csv; charset=utf-8":     "text/csv",
		"application/pdf;version=1.7": "application/pdf",
		"APPLICATION/PDF":             "application/pdf",
		"Text/CSV; charset=UTF-8":     "text/csv",
		"application/pdf":             "application/pdf",
		"":                            "",
	}
	for in, want := range cases {
		if got := normalizeContentType(in); got != want {
			t.Errorf("normalizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractText_StripsContentTypeParams(t *testing.T) {
	svc := &SubmissionsService{
		extractors: map[string]TextExtractor{"text/csv": fakeExtractor{}},
		log:        logrus.NewEntry(logrus.New()),
	}
	for _, ct := range []string{"text/csv; charset=utf-8", "TEXT/CSV", "text/csv;charset=ascii"} {
		got := svc.extractText(model.Attachment{ContentType: ct, Content: []byte("hello")})
		if got != "hello" {
			t.Errorf("%q should dispatch to the csv extractor, got %q", ct, got)
		}
	}
}

func TestIsUnreadable(t *testing.T) {
	const xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	svc := &SubmissionsService{
		cfg: &config.Config{Document: config.DocumentConfig{UnreadableMinSizeBytes: 51200, UnreadableMinChars: 100}},
		extractors: map[string]TextExtractor{
			"application/pdf": fakeExtractor{},
			xlsxType:          fakeExtractor{},
		},
	}
	cases := []struct {
		name string
		ct   string
		size int
		text string
		want bool
	}{
		{"large scanned pdf", "application/pdf", 60000, "", true},
		{"large text pdf same size", "application/pdf", 60000, strings.Repeat("real text ", 50), false},
		{"small sparse pdf below floor", "application/pdf", 1000, "", false},
		{"non-pdf with extractor, empty", xlsxType, 60000, "", false}, // only PDFs get the near-empty rule
		{"pdf with mime params", "application/pdf; version=1.7", 60000, "  \n ", true},
		{"pdf exactly at floor, empty", "application/pdf", 51200, "", true},
		{"pdf one byte below floor", "application/pdf", 51199, "", false},
		{"image, no extractor", "image/jpeg", 60000, "", true}, // can't read it: flag
		{"unsupported binary, no extractor", "application/zip", 10, "", true},
		{"plain text reads directly", "text/plain", 60000, "", false}, // text/* never flagged
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.isUnreadable(model.Attachment{ContentType: tc.ct, Size: tc.size}, tc.text)
			if got != tc.want {
				t.Errorf("isUnreadable = %v, want %v", got, tc.want)
			}
		})
	}
}
