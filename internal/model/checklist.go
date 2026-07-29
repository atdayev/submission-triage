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

// ReasonCode is the machine-readable cause an item is unsatisfied.
type ReasonCode string

const (
	ReasonNotProvided       ReasonCode = "not_provided"
	ReasonLowConfidence     ReasonCode = "low_confidence"     // a doc classified here, but below the confidence floor
	ReasonUnreadable        ReasonCode = "unreadable"         // a doc classified here, but no text extracted (scanned)
	ReasonFieldShortfall    ReasonCode = "field_shortfall"    // present, but a required field fails its rule
	ReasonEncrypted         ReasonCode = "encrypted"          // present, but password-protected; the broker can resend it unlocked
	ReasonMissingAttachment ReasonCode = "missing_attachment" // the broker referenced files but attached none
	ReasonPolicyUnknown     ReasonCode = "policy_unknown"     // no checklist matched; the broker can name the line of business
)

const (
	reasonNotProvidedText   = "document not provided"
	reasonLowConfidenceText = "received but not confidently identified"
	reasonUnreadableText    = "received but no readable text (appears scanned)"
	reasonEncryptedText     = "received but password-protected (cannot open)"
	reasonPolicyUnknownText = "awaiting clarification from sender"
)

// PolicyUnknownItemID is the synthetic item standing in for an undetermined policy type.
const PolicyUnknownItemID = "policy_unknown"

// PolicyUnknownItem is the sole outstanding item on a submission no checklist
// matched; its reason code routes it as a broker-actionable gap.
func PolicyUnknownItem() MissingItem {
	return MissingItem{
		ID:          PolicyUnknownItemID,
		Description: "Policy type not yet determined",
		Reason:      reasonPolicyUnknownText,
		Code:        ReasonPolicyUnknown,
	}
}

// MissingItem is a required item not satisfied by a submission.
type MissingItem struct {
	ID          string
	Description string
	Reason      string
	Code        ReasonCode
	// Confidence is the classifier's best guess for a low-confidence item; 0 when not applicable.
	Confidence float64 `json:",omitempty"`
}

// RequiresReview reports whether any missing item needs a human rather than the broker.
func RequiresReview(missing []MissingItem) bool {
	for _, m := range missing {
		if m.Code == ReasonUnreadable || m.Code == ReasonLowConfidence {
			return true
		}
	}
	return false
}

// BrokerActionable returns the missing items worth asking the broker for;
// unreadable/low-confidence items are the agency's, omitted.
func BrokerActionable(missing []MissingItem) []MissingItem {
	out := make([]MissingItem, 0, len(missing))
	for _, m := range missing {
		switch m.Code {
		case ReasonNotProvided, ReasonFieldShortfall, ReasonEncrypted, ReasonMissingAttachment, ReasonPolicyUnknown:
			out = append(out, m)
		}
	}
	return out
}

// ChecklistOptions carries evaluation knobs the pure model can't read from config.
type ChecklistOptions struct {
	// ConfidenceFloor is the minimum classification confidence a document needs
	// to count toward an item; 0 disables the floor.
	ConfidenceFloor float64
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

// itemDocs buckets the documents classified to one required item.
type itemDocs struct {
	qualifying    []*Document // confidence >= floor and readable; the only docs that satisfy
	unreadable    bool        // some classified doc had no extractable text
	encrypted     bool        // some classified doc is password-protected
	lowConfidence bool        // some classified doc fell below the confidence floor
	topConfidence float64     // highest confidence among the below-floor docs
}

// EvaluateChecklist returns the items a submission is still missing. A document
// counts toward an item only if it clears the confidence floor and is readable.
func EvaluateChecklist(s Submission, c Checklist, opts ChecklistOptions) []MissingItem {
	byItem := make(map[string]*itemDocs, len(c.Required))
	for i := range s.Documents {
		d := &s.Documents[i]
		if d.ClassifiedAs == "" {
			continue
		}
		b := byItem[d.ClassifiedAs]
		if b == nil {
			b = &itemDocs{}
			byItem[d.ClassifiedAs] = b
		}
		switch {
		case d.Encrypted:
			b.encrypted = true
		case d.Unreadable:
			b.unreadable = true
		case d.Confidence < opts.ConfidenceFloor:
			b.lowConfidence = true
			b.topConfidence = math.Max(b.topConfidence, d.Confidence)
		default:
			b.qualifying = append(b.qualifying, d)
		}
	}

	missing := make([]MissingItem, 0)
	for _, item := range c.Required {
		b := byItem[item.ID]
		if b == nil || len(b.qualifying) == 0 {
			missing = append(missing, unsatisfiedItem(item, b))
			continue
		}
		if item.RequiresField == nil {
			continue
		}
		// a soft-pass (extraction not run) must not mask another document's concrete failure
		affirmed, failed := false, false
		var failReason string
		for _, doc := range b.qualifying {
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
				Code:        ReasonFieldShortfall,
			})
		}
	}
	return missing
}

// unsatisfiedItem classifies why an item with no qualifying document is missing,
// most-specific cause first.
func unsatisfiedItem(item RequiredItem, b *itemDocs) MissingItem {
	m := MissingItem{ID: item.ID, Description: item.Description}
	switch {
	case b != nil && b.encrypted:
		m.Reason, m.Code = reasonEncryptedText, ReasonEncrypted
	case b != nil && b.unreadable:
		m.Reason, m.Code = reasonUnreadableText, ReasonUnreadable
	case b != nil && b.lowConfidence:
		m.Reason, m.Code, m.Confidence = reasonLowConfidenceText, ReasonLowConfidence, b.topConfidence
	default:
		m.Reason, m.Code = reasonNotProvidedText, ReasonNotProvided
	}
	return m
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
