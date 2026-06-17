package classifier

import (
	"context"
	"fmt"

	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/pkg/glob"
)

// Classifier maps a submission to a checklist item.
type Classifier interface {
	Classify(ctx context.Context, in Input) (Result, error)
}

// Input is a submission to classify against a policy checklist.
type Input struct {
	Filename    string
	ContentType string
	BodyText    string
	PolicyType  string
	Checklist   model.Checklist
}

// Result is the matched checklist item with confidence and source.
type Result struct {
	CandidateID string
	Confidence  float64
	By          string
	Reason      string
	Usage       *llm.Usage
}

// HeuristicLLMClassifier matches by filename and content, then falls back to an LLM.
type HeuristicLLMClassifier struct {
	llmClient llm.Client
}

// NewHeuristicLLMClassifier returns a classifier that falls back to client when heuristics miss.
func NewHeuristicLLMClassifier(client llm.Client) *HeuristicLLMClassifier {
	return &HeuristicLLMClassifier{llmClient: client}
}

// Classify matches in against its checklist, using the LLM only when heuristics are ambiguous.
func (c *HeuristicLLMClassifier) Classify(ctx context.Context, in Input) (Result, error) {
	byName := matchByFilename(in)
	if r, ok := singleMatch(byName, 0.95, "filename match"); ok {
		return r, nil
	}

	byContent := matchByContent(in)
	if r, ok := singleMatch(merge(byName, byContent), 0.85, ""); ok {
		switch id := r.CandidateID; {
		case byName[id] && byContent[id]:
			r.Reason = "filename and content match"
		case byName[id]:
			r.Reason = "filename match"
		default:
			r.Reason = "content match"
		}
		return r, nil
	}

	if c.llmClient == nil {
		return Result{By: "heuristic", Reason: "no match, llm unavailable"}, nil
	}
	return c.classifyByLLM(ctx, in)
}

func (c *HeuristicLLMClassifier) classifyByLLM(ctx context.Context, in Input) (Result, error) {
	candidates := make([]llm.ClassificationCandidate, 0, len(in.Checklist.Required))
	candidateIDs := make(map[string]bool, len(in.Checklist.Required))
	for _, item := range in.Checklist.Required {
		candidates = append(candidates, llm.ClassificationCandidate{ID: item.ID, Description: item.Description})
		candidateIDs[item.ID] = true
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
	if !candidateIDs[resp.CandidateID] {
		return Result{Confidence: resp.Confidence, By: "llm", Reason: "llm returned unknown candidate id", Usage: &usage}, nil
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
