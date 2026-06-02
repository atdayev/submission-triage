package checklist

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/atdayev/submission-triage/internal/model"
)

type Store interface {
	Get(policyType string) (model.Checklist, bool)
	All() []model.Checklist
}

type YAMLStore struct {
	dir string
	log *logrus.Entry

	mu    sync.RWMutex
	byKey map[string]model.Checklist
}

type rawChecklist struct {
	Name       string        `yaml:"name"`
	PolicyType string        `yaml:"policy_type"`
	Required   []rawRequired `yaml:"required_items"`
	Escalation rawEscalation `yaml:"escalation"`
}

type rawRequired struct {
	ID            string            `yaml:"id"`
	Description   string            `yaml:"description"`
	Match         rawMatch          `yaml:"match"`
	RequiresField *rawRequiresField `yaml:"requires_field,omitempty"`
}

type rawMatch struct {
	FilenamePatterns []string `yaml:"filename_patterns"`
	ContentKeywords  []string `yaml:"content_keywords"`
}

type rawRequiresField struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"`
	MinValue *float64 `yaml:"min_value,omitempty"`
}

type rawEscalation struct {
	ThresholdHours  int    `yaml:"threshold_hours"`
	Action          string `yaml:"action"`
	DigestRecipient string `yaml:"digest_recipient"`
}

func NewYAMLStore(dir string, log *logrus.Entry) (*YAMLStore, error) {
	s := &YAMLStore{dir: dir, log: log, byKey: map[string]model.Checklist{}}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *YAMLStore) Reload() error {
	loaded := map[string]model.Checklist{}
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("checklist: read %s: %w", path, err)
		}
		var raw rawChecklist
		if err := yaml.UnmarshalStrict(body, &raw); err != nil {
			return fmt.Errorf("checklist: parse %s: %w", path, err)
		}
		if raw.PolicyType == "" {
			return fmt.Errorf("checklist: %s missing policy_type", path)
		}
		if _, dup := loaded[raw.PolicyType]; dup {
			return fmt.Errorf("checklist: %s: duplicate policy_type %q", path, raw.PolicyType)
		}
		cl := raw.toModel()
		if err := validateChecklist(cl); err != nil {
			return fmt.Errorf("checklist: %s: %w", path, err)
		}
		loaded[raw.PolicyType] = cl
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.byKey = loaded
	s.mu.Unlock()
	s.log.WithField("count", len(loaded)).Info("checklists loaded")
	return nil
}

func (s *YAMLStore) Get(policyType string) (model.Checklist, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byKey[policyType]
	return c, ok
}

func (s *YAMLStore) All() []model.Checklist {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Checklist, 0, len(s.byKey))
	for _, c := range s.byKey {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PolicyType < out[j].PolicyType })
	return out
}

// validateChecklist checks item ids and filename globs.
func validateChecklist(c model.Checklist) error {
	for _, item := range c.Required {
		if item.ID == "" {
			return fmt.Errorf("required item with empty id")
		}
		for _, p := range item.Match.FilenamePatterns {
			if _, err := filepath.Match(p, ""); err != nil {
				return fmt.Errorf("item %q: invalid filename pattern %q: %w", item.ID, p, err)
			}
		}
	}
	return nil
}

func (r rawChecklist) toModel() model.Checklist {
	out := model.Checklist{
		Name:       r.Name,
		PolicyType: r.PolicyType,
		Escalation: model.EscalationPolicy{
			ThresholdHours:  r.Escalation.ThresholdHours,
			Action:          r.Escalation.Action,
			DigestRecipient: r.Escalation.DigestRecipient,
		},
	}
	for _, item := range r.Required {
		req := model.RequiredItem{
			ID:          item.ID,
			Description: item.Description,
			Match: model.MatchRules{
				FilenamePatterns: item.Match.FilenamePatterns,
				ContentKeywords:  item.Match.ContentKeywords,
			},
		}
		if item.RequiresField != nil {
			ft := model.FieldType(strings.ToLower(item.RequiresField.Type))
			if ft != model.FieldTypeNumber && ft != model.FieldTypeString {
				ft = model.FieldTypeString
			}
			req.RequiresField = &model.RequiresField{
				Name:     item.RequiresField.Name,
				Type:     ft,
				MinValue: item.RequiresField.MinValue,
			}
		}
		out.Required = append(out.Required, req)
	}
	return out
}
