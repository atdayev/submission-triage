package classifier

import (
	"context"
	"fmt"

	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/glob"
)

type Classifier interface {
	Classify(ctx context.Context, in Input) (Result, error)
}

type Input struct {
	Filename    string
	ContentType string
	BodyText    string
	PolicyType  string
	Checklist   model.Checklist
}

type Result struct {
	CandidateID string
	Confidence  float64
	By          string
	Reason      string
	Usage       *llm.Usage
}

type HeuristicLLMClassifier struct {
	llmClient llm.Client
}

func NewHeuristicLLMClassifier(client llm.Client) *HeuristicLLMClassifier {
	return &HeuristicLLMClassifier{llmClient: client}
}

func (c *HeuristicLLMClassifier) Classify(ctx context.Context, in Input) (Result, error) {
	byName := matchByFilename(in)
	if r, ok := singleMatch(byName, 0.95, "filename match"); ok {
		return r, nil
	}

	byContent := matchByContent(in)
	if r, ok := singleMatch(merge(byName, byContent), 0.85, "filename or content match"); ok {
		return r, nil
	}

	if c.llmClient == nil {
		return Result{By: "heuristic", Reason: "no match, llm unavailable"}, nil
	}
	return c.classifyByLLM(ctx, in)
}

func (c *HeuristicLLMClassifier) classifyByLLM(ctx context.Context, in Input) (Result, error) {
	candidates := make([]llm.ClassificationCandidate, 0, len(in.Checklist.Required))
	for _, item := range in.Checklist.Required {
		candidates = append(candidates, llm.ClassificationCandidate{ID: item.ID, Description: item.Description})
	}

	resp, err := c.llmClient.Classify(ctx, llm.ClassificationRequest{
		Filename:    in.Filename,
		ContentType: in.ContentType,
		TextSample:  in.BodyText,
		Candidates:  candidates,
		PolicyType:  in.PolicyType,
	})
	if err != nil {
		return Result{By: "llm", Reason: fmt.Sprintf("llm error: %v", err)}, err
	}
	usage := resp.Usage
	if resp.CandidateID == "" || resp.CandidateID == "unknown" {
		return Result{Confidence: resp.Confidence, By: "llm", Reason: resp.Reason, Usage: &usage}, nil
	}
	return Result{CandidateID: resp.CandidateID, Confidence: resp.Confidence, By: "llm", Reason: resp.Reason, Usage: &usage}, nil
}

func matchByFilename(in Input) map[string]bool {
	matches := map[string]bool{}
	for _, item := range in.Checklist.Required {
		if glob.MatchAny(item.Match.FilenamePatterns, in.Filename) {
			matches[item.ID] = true
		}
	}
	return matches
}

func matchByContent(in Input) map[string]bool {
	matches := map[string]bool{}
	for _, item := range in.Checklist.Required {
		if len(item.Match.ContentKeywords) == 0 {
			continue
		}
		if glob.ContainsAny(item.Match.ContentKeywords, in.BodyText) {
			matches[item.ID] = true
		}
	}
	return matches
}

func merge(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func singleMatch(matches map[string]bool, confidence float64, reason string) (Result, bool) {
	if len(matches) != 1 {
		return Result{}, false
	}
	for id := range matches {
		return Result{CandidateID: id, Confidence: confidence, By: "heuristic", Reason: reason}, true
	}
	return Result{}, false
}
