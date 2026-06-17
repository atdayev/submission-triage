package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
)

type stubLLM struct {
	resp llm.ClassificationResponse
	err  error
}

func (s *stubLLM) Classify(_ context.Context, _ llm.ClassificationRequest) (llm.ClassificationResponse, error) {
	return s.resp, s.err
}

func (s *stubLLM) ExtractField(_ context.Context, _ llm.FieldExtractionRequest) (llm.FieldExtractionResponse, error) {
	return llm.FieldExtractionResponse{}, nil
}

func cglChecklist() model.Checklist {
	return model.Checklist{
		Name:       "Test",
		PolicyType: "cgl",
		Required: []model.RequiredItem{
			{ID: "acord_125", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*125*"}, ContentKeywords: []string{"ACORD 125"}}},
			{ID: "acord_126", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*126*"}, ContentKeywords: []string{"ACORD 126"}}},
			{ID: "loss_runs", Match: model.MatchRules{FilenamePatterns: []string{"*loss*run*"}, ContentKeywords: []string{"Loss Run"}}},
		},
	}
}

func newTestClassifier(client llm.Client) *HeuristicLLMClassifier {
	return NewHeuristicLLMClassifier(client)
}

func TestClassify_UniqueFilenameMatch_NoLLM(t *testing.T) {
	c := newTestClassifier(nil)
	r, err := c.Classify(context.Background(), Input{
		Filename:  "ACORD_125_Acme.pdf",
		Checklist: cglChecklist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.CandidateID != "acord_125" || r.By != "heuristic" || r.Confidence != 0.95 {
		t.Fatalf("got %+v", r)
	}
}

func TestClassify_ContentMatchOnly(t *testing.T) {
	c := newTestClassifier(nil)
	r, err := c.Classify(context.Background(), Input{
		Filename:  "random.pdf",
		BodyText:  "this document contains Loss Run history",
		Checklist: cglChecklist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.CandidateID != "loss_runs" || r.Confidence != 0.85 {
		t.Fatalf("got %+v", r)
	}
}

func TestClassify_AmbiguousNoLLMReturnsEmpty(t *testing.T) {
	c := newTestClassifier(nil)
	cl := model.Checklist{
		PolicyType: "cgl",
		Required: []model.RequiredItem{
			{ID: "a", Match: model.MatchRules{FilenamePatterns: []string{"*foo*"}}},
			{ID: "b", Match: model.MatchRules{FilenamePatterns: []string{"*foo*"}}},
		},
	}
	r, err := c.Classify(context.Background(), Input{Filename: "foo.pdf", Checklist: cl})
	if err != nil {
		t.Fatal(err)
	}
	if r.CandidateID != "" {
		t.Fatalf("expected empty CandidateID, got %+v", r)
	}
	if r.Reason != "no match, llm unavailable" {
		t.Fatalf("reason: got %q", r.Reason)
	}
}

func TestClassify_FallsBackToLLM(t *testing.T) {
	stub := &stubLLM{resp: llm.ClassificationResponse{CandidateID: "acord_125", Confidence: 0.6, Reason: "guess"}}
	c := newTestClassifier(stub)
	r, err := c.Classify(context.Background(), Input{
		Filename:  "totally_random.pdf",
		Checklist: cglChecklist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.CandidateID != "acord_125" || r.By != "llm" {
		t.Fatalf("got %+v", r)
	}
}

func TestClassify_LLMReturnsUnknown(t *testing.T) {
	stub := &stubLLM{resp: llm.ClassificationResponse{CandidateID: "unknown", Confidence: 0.2}}
	c := newTestClassifier(stub)
	r, _ := c.Classify(context.Background(), Input{Filename: "x.pdf", Checklist: cglChecklist()})
	if r.CandidateID != "" {
		t.Fatalf("unknown candidate should become empty, got %+v", r)
	}
	if r.By != "llm" {
		t.Fatalf("By: got %+v", r)
	}
}

func TestClassify_LLMReturnsCandidateNotInChecklist(t *testing.T) {
	stub := &stubLLM{resp: llm.ClassificationResponse{CandidateID: "acord_999", Confidence: 0.9}}
	c := newTestClassifier(stub)
	r, _ := c.Classify(context.Background(), Input{Filename: "x.pdf", Checklist: cglChecklist()})
	if r.CandidateID != "" {
		t.Fatalf("candidate not in checklist should become empty, got %+v", r)
	}
	if r.By != "llm" {
		t.Fatalf("By: got %+v", r)
	}
}

func TestClassify_LLMError(t *testing.T) {
	stub := &stubLLM{err: errors.New("llm down")}
	c := newTestClassifier(stub)
	r, err := c.Classify(context.Background(), Input{Filename: "x.pdf", Checklist: cglChecklist()})
	if err == nil {
		t.Fatal("expected error to bubble from llm failure")
	}
	if r.By != "llm" {
		t.Fatalf("By should be llm even on error, got %+v", r)
	}
}
