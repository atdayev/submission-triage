package checklist

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

const cglYAML = `name: Commercial General Liability
policy_type: cgl
required_items:
  - id: acord_125
    description: "ACORD 125"
    match:
      filename_patterns: ["*ACORD*125*"]
      content_keywords: ["ACORD 125"]
  - id: loss_runs
    description: "Loss runs"
    match:
      filename_patterns: ["*loss*run*"]
escalation:
  threshold_hours: 72
  action: email_digest
`

const bopYAML = `name: BOP
policy_type: bop
required_items:
  - id: acord_125
    description: "ACORD 125"
    match:
      filename_patterns: ["*application*"]
escalation:
  threshold_hours: 48
`

func writeYAML(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewYAMLStore_LoadsAndExposesChecklists(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "cgl.yaml", cglYAML)
	writeYAML(t, dir, "bop.yaml", bopYAML)
	writeYAML(t, dir, "ignore.txt", "not yaml")

	store, err := NewYAMLStore(dir, logrus.NewEntry(logrus.New()))
	if err != nil {
		t.Fatalf("NewYAMLStore: %v", err)
	}

	if len(store.All()) != 2 {
		t.Fatalf("All: got %d", len(store.All()))
	}

	cgl, ok := store.Get("cgl")
	if !ok {
		t.Fatal("cgl not found")
	}
	if cgl.Name != "Commercial General Liability" {
		t.Errorf("name: %q", cgl.Name)
	}
	if len(cgl.Required) != 2 {
		t.Errorf("required: got %d", len(cgl.Required))
	}
	if cgl.Escalation.ThresholdHours != 72 {
		t.Errorf("escalation: %d", cgl.Escalation.ThresholdHours)
	}

	if _, ok := store.Get("unknown"); ok {
		t.Fatal("Get(unknown) should return false")
	}
}

func TestNewYAMLStore_MissingPolicyTypeRejected(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "bad.yaml", `name: NoPolicyType
required_items: []
`)

	_, err := NewYAMLStore(dir, logrus.NewEntry(logrus.New()))
	if err == nil {
		t.Fatal("expected error for missing policy_type")
	}
}

func TestNewYAMLStore_MalformedYAMLRejected(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "bad.yaml", `: not valid yaml: : :`)

	_, err := NewYAMLStore(dir, logrus.NewEntry(logrus.New()))
	if err == nil {
		t.Fatal("expected error for malformed yaml")
	}
}

func TestReload_PicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "cgl.yaml", cglYAML)

	store, err := NewYAMLStore(dir, logrus.NewEntry(logrus.New()))
	if err != nil {
		t.Fatal(err)
	}
	if len(store.All()) != 1 {
		t.Fatalf("initial: got %d", len(store.All()))
	}

	writeYAML(t, dir, "bop.yaml", bopYAML)
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(store.All()) != 2 {
		t.Fatalf("after reload: got %d", len(store.All()))
	}
}
