package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/pkg/retry"
)

const (
	anthropicAPIURL    = "https://api.anthropic.com/v1/messages"
	anthropicVersion   = "2023-06-01"
	maxTextSampleBytes = 2048
)

// USD per million tokens; estimate only. Unmapped model → cost 0 + a warn.
var modelPricing = map[string]pricing{
	"claude-haiku-4-5":          {input: 1.0, output: 5.0},
	"claude-haiku-4-5-20251001": {input: 1.0, output: 5.0},
	"claude-sonnet-4-6":         {input: 3.0, output: 15.0},
	"claude-opus-4-8":           {input: 15.0, output: 75.0},
}

type pricing struct {
	input  float64
	output float64
}

type Client interface {
	Classify(ctx context.Context, req ClassificationRequest) (ClassificationResponse, error)
	ExtractField(ctx context.Context, req FieldExtractionRequest) (FieldExtractionResponse, error)
}

type ClassificationRequest struct {
	Filename    string
	ContentType string
	TextSample  string
	Candidates  []ClassificationCandidate
	PolicyType  string
}

type ClassificationCandidate struct {
	ID          string
	Description string
}

type ClassificationResponse struct {
	CandidateID string  `json:"candidate_id"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
	Usage       Usage   `json:"-"`
}

type FieldExtractionRequest struct {
	Filename         string
	TextSample       string
	FieldName        string
	FieldDescription string
	FieldType        string // "number" or "string"
}

type FieldExtractionResponse struct {
	Value      any
	Confidence float64
	Reason     string
	Usage      Usage
}

type Usage struct {
	PromptHash       string
	LatencyMs        int64
	InputTokens      int
	OutputTokens     int
	EstimatedCostUSD float64
	Model            string
}

type AnthropicClient struct {
	cfg           config.AnthropicConfig
	httpClient    *http.Client
	endpoint      string
	retryAttempts int
	retryBase     time.Duration
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	Messages   []anthropicMessage   `json:"messages"`
	System     string               `json:"system,omitempty"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

type anthropicResponseContent struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	Content []anthropicResponseContent `json:"content"`
	Usage   anthropicUsage             `json:"usage"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func NewAnthropicClient(cfg config.AnthropicConfig, retryAttempts int, retryBase time.Duration, log *logrus.Entry) *AnthropicClient {
	if _, ok := modelPricing[cfg.Model]; !ok && log != nil {
		log.WithField("model", cfg.Model).
			Warn("anthropic: no pricing entry for model; EstimatedCostUSD in audit will be 0")
	}
	return &AnthropicClient{
		cfg:           cfg,
		httpClient:    &http.Client{Timeout: cfg.Timeout()},
		endpoint:      anthropicAPIURL,
		retryAttempts: retryAttempts,
		retryBase:     retryBase,
	}
}

func (c *AnthropicClient) Classify(ctx context.Context, req ClassificationRequest) (ClassificationResponse, error) {
	var resp ClassificationResponse
	if len(req.Candidates) == 0 {
		return resp, errors.New("anthropic: no candidates")
	}

	var candidateLines []string
	for _, cand := range req.Candidates {
		candidateLines = append(candidateLines, fmt.Sprintf("- %s: %s", cand.ID, cand.Description))
	}
	prompt := fmt.Sprintf(
		"You classify insurance submission documents. Choose the candidate that best matches the document.\n"+
			"Policy type: %s\nFilename: %s\nContent type: %s\nText sample:\n%s\n\nCandidates:\n%s\n\n"+
			"Return JSON: {\"candidate_id\":\"...\",\"confidence\":0.0-1.0,\"reason\":\"...\"}. "+
			"If none fit, set candidate_id to \"unknown\".",
		req.PolicyType, req.Filename, req.ContentType, truncate(req.TextSample, maxTextSampleBytes),
		strings.Join(candidateLines, "\n"),
	)

	raw, usage, err := c.callMessages(ctx, anthropicRequest{
		Model:     c.cfg.Model,
		MaxTokens: c.cfg.MaxTokens,
		System:    "Respond with JSON only, no prose, no code fences.",
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
	}, prompt)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &resp); err != nil {
		return resp, fmt.Errorf("anthropic: parse classify response: %w", err)
	}
	resp.Usage = usage
	return resp, nil
}

func (c *AnthropicClient) ExtractField(ctx context.Context, req FieldExtractionRequest) (FieldExtractionResponse, error) {
	if req.FieldName == "" {
		return FieldExtractionResponse{}, errors.New("anthropic: field name required")
	}
	fieldType := strings.ToLower(req.FieldType)
	if fieldType != "number" && fieldType != "string" {
		fieldType = "string"
	}
	prompt := fmt.Sprintf(
		"Extract the field %q (%s) from the following document.\n"+
			"Description: %s\nFilename: %s\nText sample:\n%s\n\n"+
			"Call the report_field tool with the value. If the field is not present, "+
			"call the tool with a null value.",
		req.FieldName, fieldType, req.FieldDescription, req.Filename,
		truncate(req.TextSample, maxTextSampleBytes),
	)
	tool := anthropicTool{
		Name:        "report_field",
		Description: "Report the extracted field value from the document.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value":      map[string]any{"type": []string{fieldType, "null"}},
				"confidence": map[string]any{"type": "number"},
				"reason":     map[string]any{"type": "string"},
			},
			"required": []string{"value"},
		},
	}

	body, usage, err := c.callMessages(ctx, anthropicRequest{
		Model:     c.cfg.Model,
		MaxTokens: c.cfg.MaxTokens,
		System:    "You extract structured fields from insurance documents. Use the report_field tool.",
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
		Tools:     []anthropicTool{tool},
		ToolChoice: &anthropicToolChoice{
			Type: "tool",
			Name: "report_field",
		},
	}, prompt)
	if err != nil {
		return FieldExtractionResponse{Usage: usage}, err
	}
	var parsed struct {
		Value      any     `json:"value"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return FieldExtractionResponse{Usage: usage}, fmt.Errorf("anthropic: parse extract response: %w", err)
	}
	return FieldExtractionResponse{
		Value:      parsed.Value,
		Confidence: parsed.Confidence,
		Reason:     parsed.Reason,
		Usage:      usage,
	}, nil
}

func (c *AnthropicClient) callMessages(ctx context.Context, body anthropicRequest, promptForHash string) (string, Usage, error) {
	usage := Usage{Model: c.cfg.Model}
	if c.cfg.APIKey == "" {
		return "", usage, errors.New("anthropic: api key not configured")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", usage, fmt.Errorf("anthropic: marshal: %w", err)
	}

	sum := sha256.Sum256([]byte(promptForHash))
	usage.PromptHash = hex.EncodeToString(sum[:])
	start := time.Now()

	var text string
	err = retry.Do(ctx, c.retryAttempts, c.retryBase, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
		if err != nil {
			return retry.MarkPermanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.cfg.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		// status decides retry/permanent before decoding; a 4xx/5xx body may not be JSON
		if resp.StatusCode >= 500 {
			return fmt.Errorf("anthropic: server error %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("anthropic: rate limited %d", resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			msg := ""
			var errResp anthropicResponse
			if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != nil {
				msg = errResp.Error.Message
			}
			return retry.MarkPermanent(fmt.Errorf("anthropic: client error %d: %s", resp.StatusCode, msg))
		}

		var parsed anthropicResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return fmt.Errorf("anthropic: decode: %w", err)
		}

		usage.InputTokens = parsed.Usage.InputTokens
		usage.OutputTokens = parsed.Usage.OutputTokens

		// Prefer tool_use block if present; otherwise concatenate text blocks.
		for _, block := range parsed.Content {
			if block.Type == "tool_use" && block.Input != nil {
				toolJSON, err := json.Marshal(block.Input)
				if err != nil {
					return retry.MarkPermanent(fmt.Errorf("anthropic: re-encode tool_use: %w", err))
				}
				text = string(toolJSON)
				return nil
			}
		}
		var b strings.Builder
		for _, block := range parsed.Content {
			if block.Type == "text" {
				b.WriteString(block.Text)
			}
		}
		text = b.String()
		return nil
	})
	usage.LatencyMs = time.Since(start).Milliseconds()
	if p, ok := modelPricing[c.cfg.Model]; ok {
		usage.EstimatedCostUSD = (float64(usage.InputTokens)*p.input + float64(usage.OutputTokens)*p.output) / 1_000_000
	}
	if err != nil {
		return "", usage, err
	}
	return text, usage, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}
