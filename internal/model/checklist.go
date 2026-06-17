package model

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Checklist is the set of required documents for a policy type.
type Checklist struct {
	Name       string
	PolicyType string
	Required   []RequiredItem
	Escalation EscalationPolicy
}

// MissingItem is a required item not satisfied by a submission.
type MissingItem struct {
	ID          string
	Description string
	Reason      string
}

// RequiredItem is one document a checklist requires.
type RequiredItem struct {
	ID            string
	Description   string
	Match         MatchRules
	RequiresField *RequiresField
}

// MatchRules classify a document to a required item.
type MatchRules struct {
	FilenamePatterns []string
	ContentKeywords  []string
}

// EscalationPolicy controls when a stalled submission escalates.
type EscalationPolicy struct {
	ThresholdHours  int
	Action          string
	DigestRecipient string
}

// FieldType is the expected type of an extracted field.
type FieldType string

const (
	FieldTypeNumber FieldType = "number"
	FieldTypeString FieldType = "string"
)

// RequiresField asserts a constraint on an extracted field.
type RequiresField struct {
	Name     string
	Type     FieldType
	MinValue *float64
	// Unit is the human noun for the value (e.g. "years").
	Unit string
}

// EvaluateChecklist returns the items a submission is still missing.
func EvaluateChecklist(s Submission, c Checklist) []MissingItem {
	docsByItem := make(map[string][]*Document, len(c.Required))
	for i := range s.Documents {
		d := &s.Documents[i]
		if d.ClassifiedAs == "" {
			continue
		}
		docsByItem[d.ClassifiedAs] = append(docsByItem[d.ClassifiedAs], d)
	}

	missing := make([]MissingItem, 0)
	for _, item := range c.Required {
		docs := docsByItem[item.ID]
		if len(docs) == 0 {
			missing = append(missing, MissingItem{
				ID:          item.ID,
				Description: item.Description,
				Reason:      "document not provided",
			})
			continue
		}
		if item.RequiresField == nil {
			continue
		}
		// a soft-pass (extraction not run) must not mask another document's concrete failure
		affirmed, failed := false, false
		var failReason string
		for _, doc := range docs {
			reason, ok, ran := evaluateField(item.RequiresField, doc)
			if ok && ran {
				affirmed = true
				break
			}
			if !ok {
				failed = true
				failReason = reason
			}
		}
		if !affirmed && failed {
			missing = append(missing, MissingItem{
				ID:          item.ID,
				Description: item.Description,
				Reason:      failReason,
			})
		}
	}
	return missing
}

// evaluateField reports (reason, ok, ran); ran is false on a soft-pass (nil map
// or absent key), and reasons are customer-facing copy with no field jargon.
func evaluateField(rf *RequiresField, doc *Document) (reason string, ok, ran bool) {
	if doc.ExtractedFields == nil {
		return "", true, false
	}
	raw, present := doc.ExtractedFields[rf.Name]
	if !present {
		return "", true, false
	}
	if raw == nil {
		return unconfirmedReason(rf.Unit), false, true
	}
	if rf.MinValue != nil {
		f, valid := numericValue(raw)
		if !valid {
			return unconfirmedReason(rf.Unit), false, true
		}
		if f < *rf.MinValue {
			return shortfallReason(rf.Unit, f, *rf.MinValue), false, true
		}
	} else if rf.Type == FieldTypeNumber {
		if _, valid := numericValue(raw); !valid {
			return unconfirmedReason(rf.Unit), false, true
		}
	}
	return "", true, true
}

// shortfallReason explains a below-minimum numeric value in plain language.
func shortfallReason(unit string, have, need float64) string {
	if unit != "" {
		return fmt.Sprintf("covers only %s %s, need at least %s", formatNum(have), unit, formatNum(need))
	}
	return fmt.Sprintf("only %s provided, need at least %s", formatNum(have), formatNum(need))
}

// formatNum renders a value as plain decimal, never scientific notation.
func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// unconfirmedReason explains that extraction ran but produced no usable value.
func unconfirmedReason(unit string) string {
	if unit != "" {
		return fmt.Sprintf("could not confirm the number of %s covered", unit)
	}
	return "could not confirm the required detail on this document"
}

func numericValue(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
