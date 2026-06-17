package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

func testLog() *logrus.Entry {
	lg := logrus.New()
	lg.SetOutput(io.Discard)
	return logrus.NewEntry(lg)
}

func testClient(t *testing.T, url string) *AnthropicClient {
	t.Helper()
	c := NewAnthropicClient(config.AnthropicConfig{
		APIKey:     "test-key",
		Model:      "claude-haiku-4-5",
		MaxTokens:  256,
		TimeoutSec: 5,
	}, 3, time.Millisecond, testLog())
	c.endpoint = url
	return c
}

func TestAnthropic_Classify_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key: %v", r.Header)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"claude-haiku-4-5"`) {
			t.Errorf("model not in body: %s", body)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"candidate_id":"acord_125","confidence":0.92,"reason":"matches title"}`},
			},
			"usage": map[string]int{"input_tokens": 150, "output_tokens": 25},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.Classify(context.Background(), ClassificationRequest{
		Filename:   "ACORD_125.pdf",
		PolicyType: "cgl",
		Candidates: []ClassificationCandidate{{ID: "acord_125", Description: "ACORD 125"}},
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.CandidateID != "acord_125" || resp.Confidence != 0.92 {
		t.Errorf("parsed: %+v", resp)
	}
	if resp.Usage.PromptHash == "" {
		t.Error("PromptHash not set")
	}
	if resp.Usage.InputTokens != 150 || resp.Usage.OutputTokens != 25 {
		t.Errorf("tokens: %+v", resp.Usage)
	}
	if resp.Usage.Model != "claude-haiku-4-5" {
		t.Errorf("model: got %q", resp.Usage.Model)
	}
	if resp.Usage.LatencyMs < 0 {
		t.Errorf("latency: %d", resp.Usage.LatencyMs)
	}
}

func TestAnthropic_Classify_CostCalculation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"candidate_id":"x","confidence":1.0}`}},
			"usage":   map[string]int{"input_tokens": 1_000_000, "output_tokens": 1_000_000},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, _ := c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
	// Haiku 4.5: $1 input + $5 output per MTok → 1.0 + 5.0 = 6.0 USD
	if resp.Usage.EstimatedCostUSD != 6.0 {
		t.Errorf("cost: got %v, want 6.0", resp.Usage.EstimatedCostUSD)
	}
}

func TestAnthropic_ExtractField_ToolUseHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"tool_choice"`) {
			t.Errorf("expected tool_choice in body, got: %s", body)
		}
		if !strings.Contains(string(body), `"report_field"`) {
			t.Errorf("expected report_field tool, got: %s", body)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"name":  "report_field",
					"input": map[string]any{"value": 7.0, "confidence": 0.9, "reason": "header says 7 years"},
				},
			},
			"usage": map[string]int{"input_tokens": 200, "output_tokens": 10},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.ExtractField(context.Background(), FieldExtractionRequest{
		Filename:  "loss_runs.pdf",
		FieldName: "years_covered",
		FieldType: "number",
	})
	if err != nil {
		t.Fatalf("ExtractField: %v", err)
	}
	if resp.Value != 7.0 {
		t.Errorf("value: got %v, want 7.0", resp.Value)
	}
	if resp.Confidence != 0.9 {
		t.Errorf("confidence: got %v", resp.Confidence)
	}
	if resp.Usage.InputTokens != 200 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestAnthropic_429_Retried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"{\"candidate_id\":\"x\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if resp.CandidateID != "x" {
		t.Errorf("got %+v", resp)
	}
	if hits.Load() < 2 {
		t.Errorf("expected ≥2 hits (retry), got %d", hits.Load())
	}
}

func TestAnthropic_4xx_NotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"auth","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 hit (permanent), got %d", hits.Load())
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401: %v", err)
	}
}

// A 4xx whose body is not JSON (gateway HTML, empty) must still be treated as
// permanent, not retried as a decode error.
func TestAnthropic_4xxNonJSONBody_NotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("<html><body>400 Bad Request</body></html>"))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 hit (permanent), got %d", hits.Load())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400, got: %v", err)
	}
}

func TestAnthropic_EmptyAPIKey_FailsBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	c := NewAnthropicClient(config.AnthropicConfig{
		Model: "claude-haiku-4-5", TimeoutSec: 1, MaxTokens: 1,
	}, 1, time.Millisecond, testLog())
	c.endpoint = srv.URL
	_, err := c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
	if err == nil {
		t.Fatal("expected error for empty api key")
	}
	if !strings.Contains(err.Error(), "api key") {
		t.Errorf("error should mention api key: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("should not have made request, got %d hits", hits.Load())
	}
}

func TestAnthropic_UnknownModel_WarnsAtStartup(t *testing.T) {
	lg, hook := newCaptureLogger()
	NewAnthropicClient(config.AnthropicConfig{
		APIKey: "x", Model: "claude-fictional-9000", TimeoutSec: 1, MaxTokens: 1,
	}, 1, time.Millisecond, logrus.NewEntry(lg))

	found := false
	for _, e := range hook.entries {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, "no pricing entry") {
			found = true
			if e.Data["model"] != "claude-fictional-9000" {
				t.Errorf("model field: %v", e.Data["model"])
			}
		}
	}
	if !found {
		t.Error("expected pricing warning at startup for unknown model")
	}
}

func TestAnthropic_KnownModel_DoesNotWarn(t *testing.T) {
	lg, hook := newCaptureLogger()
	NewAnthropicClient(config.AnthropicConfig{
		APIKey: "x", Model: "claude-haiku-4-5", TimeoutSec: 1, MaxTokens: 1,
	}, 1, time.Millisecond, logrus.NewEntry(lg))

	for _, e := range hook.entries {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, "no pricing entry") {
			t.Errorf("known model should not warn: %+v", e)
		}
	}
}

type captureHook struct {
	entries []*logrus.Entry
}

func (h *captureHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h *captureHook) Fire(e *logrus.Entry) error {
	h.entries = append(h.entries, e)
	return nil
}

func newCaptureLogger() (*logrus.Logger, *captureHook) {
	lg := logrus.New()
	lg.SetOutput(io.Discard)
	h := &captureHook{}
	lg.AddHook(h)
	return lg, h
}

func TestAnthropic_NoCandidates_RejectedLocally(t *testing.T) {
	c := testClient(t, "http://unreachable.invalid")
	_, err := c.Classify(context.Background(), ClassificationRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no candidates") {
		t.Errorf("error should mention no candidates: %v", err)
	}
}

func classifyText(t *testing.T, text string) (ClassificationResponse, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	return c.Classify(context.Background(), ClassificationRequest{
		Candidates: []ClassificationCandidate{{ID: "x", Description: "y"}},
	})
}

func TestAnthropic_Classify_NotJSON(t *testing.T) {
	_, err := classifyText(t, "not json")
	if err == nil {
		t.Fatal("expected parse error for non-JSON output")
	}
}

func TestAnthropic_Classify_TrailingProse(t *testing.T) {
	resp, err := classifyText(t, `{"candidate_id":"acord_125","confidence":0.8} note: {x}`)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.CandidateID != "acord_125" || resp.Confidence != 0.8 {
		t.Errorf("parsed wrong object: %+v", resp)
	}
}

func TestAnthropic_Classify_ConfidenceClamped(t *testing.T) {
	for _, tc := range []struct {
		raw  float64
		want float64
	}{{5.0, 1.0}, {-1.0, 0.0}} {
		resp, err := classifyText(t, fmt.Sprintf(`{"candidate_id":"x","confidence":%v}`, tc.raw))
		if err != nil {
			t.Fatalf("Classify(%v): %v", tc.raw, err)
		}
		if resp.Confidence != tc.want {
			t.Errorf("confidence %v: got %v, want %v", tc.raw, resp.Confidence, tc.want)
		}
	}
}

func TestAnthropic_ExtractField_TextOnly_NilValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "I could not find the field."}},
			"usage":   map[string]int{"input_tokens": 5, "output_tokens": 3},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.ExtractField(context.Background(), FieldExtractionRequest{
		FieldName: "years_covered",
		FieldType: "number",
	})
	if err != nil {
		t.Fatalf("ExtractField: %v", err)
	}
	if resp.Value != nil {
		t.Errorf("value: got %v, want nil", resp.Value)
	}
	if resp.Usage.InputTokens != 5 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestAnthropic_ExtractField_NumberFromString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type":  "tool_use",
				"name":  "report_field",
				"input": map[string]any{"value": "7", "confidence": 0.9},
			}},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	resp, err := c.ExtractField(context.Background(), FieldExtractionRequest{
		FieldName: "years_covered",
		FieldType: "number",
	})
	if err != nil {
		t.Fatalf("ExtractField: %v", err)
	}
	if resp.Value != 7.0 {
		t.Errorf("value: got %v (%T), want 7.0 (float64)", resp.Value, resp.Value)
	}
}

func TestTruncate_RuneBoundary(t *testing.T) {
	// 3-byte runes: the cut at maxTextSampleBytes (a power of two) lands mid-rune,
	// so a byte-naive truncate would yield invalid UTF-8
	s := strings.Repeat("世", 2000)
	out := truncate(s, maxTextSampleBytes)
	if !utf8.ValidString(out) {
		t.Error("truncate split a multi-byte rune")
	}
	if !strings.HasSuffix(out, "...[truncated]") {
		t.Error("missing truncation marker")
	}
}
