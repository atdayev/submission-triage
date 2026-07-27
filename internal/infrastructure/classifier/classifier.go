package classifier

import (
	"context"
	"errors"
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
	Capped      bool // the LLM was skipped because the daily spend cap was reached
	Usage       *llm.Usage
}

// Heuristic match confidences; filename scores highest despite being the weakest evidence.
const (
	confidenceFilename = 0.95
	confidenceKeyword  = 0.85
)

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
	byContent := matchByContent(in)

	if id, ok := singleMatch(byName); ok {
		by, reason := model.ClassifiedByFilename, "filename match"
		if byContent[id] {
			by, reason = model.ClassifiedByFilenameKeyword, "filename and content match"
		}
		return Result{CandidateID: id, Confidence: confidenceFilename, By: by, Reason: reason}, nil
	}
	// byName isn't a singleton here; a filename hit would make merge ambiguous, so a lone match is keyword-only
	if id, ok := singleMatch(merge(byName, byContent)); ok {
		return Result{CandidateID: id, Confidence: confidenceKeyword, By: model.ClassifiedByKeyword, Reason: "content match"}, nil
	}

	if c.llmClient == nil {
		return Result{By: model.ClassifiedByHeuristic, Reason: "no match, llm unavailable"}, nil
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
	if errors.Is(err, llm.ErrSpendCapReached) {
		// spend cap reached: degrade to heuristic-only, exactly like an absent LLM
		return Result{By: model.ClassifiedByHeuristic, Reason: "llm spend cap reached", Capped: true}, nil
	}
	if err != nil {
		// a call that spent tokens then failed to parse still carries usage; surface
		// it so the cost is audited like the extract-field path
		usage := resp.Usage
		return Result{By: model.ClassifiedByLLM, Reason: fmt.Sprintf("llm error: %v", err), Usage: &usage}, err
	}
	usage := resp.Usage
	if resp.CandidateID == "" || resp.CandidateID == "unknown" {
		return Result{Confidence: resp.Confidence, By: model.ClassifiedByLLM, Reason: resp.Reason, Usage: &usage}, nil
	}
	if !candidateIDs[resp.CandidateID] {
		return Result{Confidence: resp.Confidence, By: model.ClassifiedByLLM, Reason: "llm returned unknown candidate id", Usage: &usage}, nil
	}
	return Result{CandidateID: resp.CandidateID, Confidence: resp.Confidence, By: model.ClassifiedByLLM, Reason: resp.Reason, Usage: &usage}, nil
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

// singleMatch returns the sole matched id, or false when zero or several matched.
func singleMatch(matches map[string]bool) (string, bool) {
	if len(matches) != 1 {
		return "", false
	}
	for id := range matches {
		return id, true
	}
	return "", false
}
