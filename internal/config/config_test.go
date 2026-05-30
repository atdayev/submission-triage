package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_RejectsMissingPort(t *testing.T) {
	cfg := &Config{
		Database:   DatabaseConfig{Path: "x"},
		Checklists: ChecklistsConfig{Directory: "y"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "http.port") {
		t.Fatalf("expected port error, got %v", err)
	}
}

func TestValidate_RejectsMissingDBPath(t *testing.T) {
	cfg := &Config{
		HTTP:       HTTPConfig{Port: 8080},
		Checklists: ChecklistsConfig{Directory: "y"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "database.path") {
		t.Fatalf("expected db.path error, got %v", err)
	}
}

func TestValidate_RejectsMissingChecklists(t *testing.T) {
	cfg := &Config{
		HTTP:     HTTPConfig{Port: 8080},
		Database: DatabaseConfig{Path: "x"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "checklists.directory") {
		t.Fatalf("expected checklists error, got %v", err)
	}
}

func TestValidate_AcceptsMinimalConfig(t *testing.T) {
	cfg := &Config{
		HTTP:       HTTPConfig{Port: 8080},
		Database:   DatabaseConfig{Path: "x"},
		Checklists: ChecklistsConfig{Directory: "y"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestApplyDefaults_FillsMissing(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Service.Name != "submission-triage" {
		t.Errorf("service.name: %q", cfg.Service.Name)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("http.port: %d", cfg.HTTP.Port)
	}
	if cfg.Database.Path == "" {
		t.Error("database.path: should default")
	}
	if cfg.Anthropic.Model == "" {
		t.Error("anthropic.model: should default")
	}
	if cfg.Escalation.ThresholdHours != 72 {
		t.Errorf("escalation.threshold_hours: %d", cfg.Escalation.ThresholdHours)
	}
	if cfg.Retry.Attempts != 3 {
		t.Errorf("retry.attempts: %d", cfg.Retry.Attempts)
	}
}

func TestLoad_ExpandsEnvAndAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	contents := `service:
  name: submission-triage
http:
  port: ${TEST_PORT}
database:
  path: ${TEST_DB_PATH}
checklists:
  directory: ./checklists
`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PORT", "9090")
	t.Setenv("TEST_DB_PATH", "./data/test.db")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Port != 9090 {
		t.Errorf("port: got %d, want 9090", cfg.HTTP.Port)
	}
	if cfg.Database.Path != "./data/test.db" {
		t.Errorf("db.path: got %q", cfg.Database.Path)
	}
	if cfg.Anthropic.Model == "" {
		t.Error("anthropic.model: defaults not applied")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEscalationConfig_DurationConversions(t *testing.T) {
	e := EscalationConfig{
		IntervalMinutes:     15,
		ThresholdHours:      72,
		AutoCloseAfterHours: 336,
		DigestIntervalHours: 24,
	}
	if e.Interval().Minutes() != 15 {
		t.Errorf("Interval: got %v", e.Interval())
	}
	if e.Threshold().Hours() != 72 {
		t.Errorf("Threshold: got %v", e.Threshold())
	}
	if e.AutoCloseAfter().Hours() != 336 {
		t.Errorf("AutoCloseAfter: got %v", e.AutoCloseAfter())
	}
	if e.DigestInterval().Hours() != 24 {
		t.Errorf("DigestInterval: got %v", e.DigestInterval())
	}
}

func TestApplyDefaults_FillsEscalationExtras(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if cfg.Escalation.AutoCloseAfterHours == 0 {
		t.Error("auto_close_after_hours: should default")
	}
	if cfg.Escalation.DigestIntervalHours == 0 {
		t.Error("digest_interval_hours: should default")
	}
}

func TestApplyDefaults_FillsIMAPandSMTP(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if cfg.IMAP.Mailbox != "INBOX" {
		t.Errorf("imap.mailbox default: got %q", cfg.IMAP.Mailbox)
	}
	if cfg.IMAP.Port != "993" {
		t.Errorf("imap.port default: got %q", cfg.IMAP.Port)
	}
	if cfg.IMAP.PollIntervalSeconds != 30 {
		t.Errorf("imap.poll_interval_seconds default: got %d", cfg.IMAP.PollIntervalSeconds)
	}
	if cfg.SMTP.Port != "587" {
		t.Errorf("smtp.port default: got %q", cfg.SMTP.Port)
	}
}

func TestIMAPConfigured(t *testing.T) {
	if (IMAPConfig{Host: "imap.gmail.com"}).Configured() {
		t.Error("host alone should not count as configured")
	}
	full := IMAPConfig{Host: "imap.gmail.com", Username: "u", Password: "p"}
	if !full.Configured() {
		t.Error("host+username+password should be configured")
	}
}

func TestSMTPConfigured(t *testing.T) {
	if (SMTPConfig{Host: "smtp.gmail.com"}).Configured() {
		t.Error("host without from_address should not count as configured")
	}
	if !(SMTPConfig{Host: "smtp.gmail.com", FromAddress: "ops@x"}).Configured() {
		t.Error("host+from_address should be configured")
	}
}
