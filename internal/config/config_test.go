package config

import (
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
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
		Escalation: EscalationConfig{IntervalMinutes: 15},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_RejectsNonPositiveIntervals(t *testing.T) {
	base := func() *Config {
		return &Config{
			HTTP:       HTTPConfig{Port: 8080},
			Database:   DatabaseConfig{Path: "x"},
			Checklists: ChecklistsConfig{Directory: "y"},
			Escalation: EscalationConfig{IntervalMinutes: 15},
		}
	}

	c := base()
	c.Escalation.IntervalMinutes = 0
	if err := c.Validate(); err == nil {
		t.Error("escalation interval 0 should be rejected (NewTicker would panic)")
	}

	c = base()
	c.IMAP = IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 0}
	if err := c.Validate(); err == nil {
		t.Error("imap poll interval 0 with IMAP configured should be rejected")
	}

	// poll interval 0 is harmless when IMAP is not configured (poller never starts)
	c = base()
	c.IMAP = IMAPConfig{PollIntervalSeconds: 0}
	if err := c.Validate(); err != nil {
		t.Errorf("unconfigured IMAP should not trip poll-interval validation: %v", err)
	}
}

// parseEnv parses Config from an explicit env map so defaults are deterministic.
func parseEnv(t *testing.T, vars map[string]string) *Config {
	t.Helper()
	var cfg Config
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: vars}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &cfg
}

func TestParse_AppliesTagDefaults(t *testing.T) {
	cfg := parseEnv(t, map[string]string{})

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
	if cfg.Escalation.AutoCloseAfterHours != 336 {
		t.Errorf("escalation.auto_close_after_hours: %d", cfg.Escalation.AutoCloseAfterHours)
	}
	if cfg.Retry.Attempts != 3 {
		t.Errorf("retry.attempts: %d", cfg.Retry.Attempts)
	}
	if cfg.IMAP.Mailbox != "INBOX" {
		t.Errorf("imap.mailbox default: got %q", cfg.IMAP.Mailbox)
	}
	if cfg.IMAP.Port != "993" {
		t.Errorf("imap.port default: got %q", cfg.IMAP.Port)
	}
	if cfg.IMAP.PollIntervalSeconds != 30 {
		t.Errorf("imap.poll_interval_seconds default: got %d", cfg.IMAP.PollIntervalSeconds)
	}
	if cfg.IMAP.MaxMessageMB != 32 {
		t.Errorf("imap.max_message_mb default: got %d", cfg.IMAP.MaxMessageMB)
	}
	if cfg.SMTP.Port != "587" {
		t.Errorf("smtp.port default: got %q", cfg.SMTP.Port)
	}
}

func TestParse_ReadsEnvOverrides(t *testing.T) {
	cfg := parseEnv(t, map[string]string{
		"HTTP_PORT":         "9090",
		"DB_PATH":           "./data/test.db",
		"IMAP_HOST":         "imap.gmail.com",
		"IMAP_PORT":         "1993",
		"OUTBOUND_PROVIDER": "smtp",
	})

	if cfg.HTTP.Port != 9090 {
		t.Errorf("port: got %d, want 9090", cfg.HTTP.Port)
	}
	if cfg.Database.Path != "./data/test.db" {
		t.Errorf("db.path: got %q", cfg.Database.Path)
	}
	if cfg.IMAP.Port != "1993" {
		t.Errorf("imap.port override: got %q", cfg.IMAP.Port)
	}
	if cfg.Outbound.Provider != "smtp" {
		t.Errorf("outbound.provider: got %q", cfg.Outbound.Provider)
	}
}

func TestParse_SMTPFromName(t *testing.T) {
	// Defaults when unset.
	cfg := parseEnv(t, map[string]string{})
	if cfg.SMTP.FromName != "Submission Triage" {
		t.Errorf("smtp.from_name default: got %q", cfg.SMTP.FromName)
	}

	// An explicit SMTP from-name wins.
	cfg = parseEnv(t, map[string]string{"SMTP_FROM_NAME": "Acme Sender"})
	if cfg.SMTP.FromName != "Acme Sender" {
		t.Errorf("smtp.from_name explicit: got %q", cfg.SMTP.FromName)
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
