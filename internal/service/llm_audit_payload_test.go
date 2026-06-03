package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// asserts the EventLLMCall payload carries prompt hash, latency, tokens, and cost
func TestIngestEmail_LLMCallAuditPayload_Shape(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := cglChecklistWithLossRuns()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}
	lm := &fakeLLM{
		extractResp: llm.FieldExtractionResponse{
			Value: 6.0,
			Usage: llm.Usage{
				PromptHash:       "abc123",
				LatencyMs:        42,
				InputTokens:      120,
				OutputTokens:     30,
				EstimatedCostUSD: 0.000270,
				Model:            "claude-haiku-4-5",
			},
		},
	}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	var llmCallPayloads []map[string]any
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventLLMCall {
			llmCallPayloads = append(llmCallPayloads, e.Payload)
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, lm)
	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-audit",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "loss_runs.pdf", ContentType: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()

	if len(llmCallPayloads) != 1 {
		t.Fatalf("want 1 EventLLMCall, got %d", len(llmCallPayloads))
	}
	p := llmCallPayloads[0]
	required := []string{"op", "filename", "model", "prompt_hash", "latency_ms", "input_tokens", "output_tokens", "estimated_cost_usd"}
	for _, k := range required {
		if _, ok := p[k]; !ok {
			t.Errorf("EventLLMCall payload missing key %q: payload=%+v", k, p)
		}
	}
	if p["op"] != "extract_field" {
		t.Errorf("op: got %v, want extract_field", p["op"])
	}
	if p["prompt_hash"] != "abc123" {
		t.Errorf("prompt_hash: got %v", p["prompt_hash"])
	}
	if p["input_tokens"] != 120 {
		t.Errorf("input_tokens: got %v (%T)", p["input_tokens"], p["input_tokens"])
	}
	if p["model"] != "claude-haiku-4-5" {
		t.Errorf("model: got %v", p["model"])
	}
}
