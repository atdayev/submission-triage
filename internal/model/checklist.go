package model

import "fmt"

type Checklist struct {
	Name       string
	PolicyType string
	Required   []RequiredItem
	Escalation EscalationPolicy
}

type MissingItem struct {
	ID          string
	Description string
	Reason      string
}

type RequiredItem struct {
	ID            string
	Description   string
	Match         MatchRules
	RequiresField *RequiresField
}

type MatchRules struct {
	FilenamePatterns []string
	ContentKeywords  []string
}

type EscalationPolicy struct {
	ThresholdHours  int
	Action          string
	DigestRecipient string
}

type FieldType string

const (
	FieldTypeNumber FieldType = "number"
	FieldTypeString FieldType = "string"
)

type RequiresField struct {
	Name     string
	Type     FieldType
	MinValue *float64
}

func EvaluateChecklist(s Submission, c Checklist) []MissingItem {
	docsByItem := make(map[string]*Document, len(c.Required))
	for i := range s.Documents {
		d := &s.Documents[i]
		if d.ClassifiedAs == "" {
			continue
		}
		if _, ok := docsByItem[d.ClassifiedAs]; ok {
			continue
		}
		docsByItem[d.ClassifiedAs] = d
	}

	missing := make([]MissingItem, 0)
	for _, item := range c.Required {
		doc, ok := docsByItem[item.ID]
		if !ok {
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
		reason, ok := evaluateField(item.RequiresField, doc)
		if !ok {
			missing = append(missing, MissingItem{
				ID:          item.ID,
				Description: item.Description,
				Reason:      reason,
			})
		}
	}
	return missing
}

// soft-pass when extraction didn't run (nil map / absent key). A present
// nil means it ran and the field's missing → fail. Else validate.
func evaluateField(rf *RequiresField, doc *Document) (string, bool) {
	if doc.ExtractedFields == nil {
		return "", true
	}
	raw, present := doc.ExtractedFields[rf.Name]
	if !present {
		return "", true
	}
	if raw == nil {
		return fmt.Sprintf("field %q not found in document", rf.Name), false
	}
	if rf.MinValue != nil {
		f, ok := numericValue(raw)
		if !ok {
			return fmt.Sprintf("field %q is not numeric", rf.Name), false
		}
		if f < *rf.MinValue {
			return fmt.Sprintf("field %q is %v, needs at least %v", rf.Name, f, *rf.MinValue), false
		}
	}
	return "", true
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
