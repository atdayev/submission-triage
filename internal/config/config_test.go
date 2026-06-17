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
		Log:        LogConfig{RotationHours: 24},
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
			Log:        LogConfig{RotationHours: 24},
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

func TestValidate_RejectsPortOutOfRange(t *testing.T) {
	base := func() *Config {
		return &Config{
			HTTP:       HTTPConfig{Port: 8080},
			Database:   DatabaseConfig{Path: "x"},
			Checklists: ChecklistsConfig{Directory: "y"},
			Log:        LogConfig{RotationHours: 24},
			Escalation: EscalationConfig{IntervalMinutes: 15},
		}
	}

	c := base()
	c.HTTP.Port = 70000
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "http.port") {
		t.Errorf("http.port 70000 should be rejected, got %v", err)
	}

	c = base()
	c.IMAP = IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 30, Port: "0"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "imap.port") {
		t.Errorf("imap.port 0 should be rejected, got %v", err)
	}

	c = base()
	c.IMAP = IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 30, Port: "abc"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "imap.port") {
		t.Errorf("non-numeric imap.port should be rejected, got %v", err)
	}

	c = base()
	c.SMTP = SMTPConfig{Host: "smtp.x", FromAddress: "ops@x", Port: "99999"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "smtp.port") {
		t.Errorf("smtp.port 99999 should be rejected, got %v", err)
	}

	// valid ports pass
	c = base()
	c.IMAP = IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 30, Port: "993", MaxMessageMB: 32}
	c.SMTP = SMTPConfig{Host: "smtp.x", FromAddress: "ops@x", Port: "587"}
	if err := c.Validate(); err != nil {
		t.Errorf("valid ports should pass: %v", err)
	}
}

func TestValidate_RejectsNegativeMaxMessageMB(t *testing.T) {
	cfg := &Config{
		HTTP:       HTTPConfig{Port: 8080},
		Database:   DatabaseConfig{Path: "x"},
		Checklists: ChecklistsConfig{Directory: "y"},
		Log:        LogConfig{RotationHours: 24},
		Escalation: EscalationConfig{IntervalMinutes: 15},
		IMAP:       IMAPConfig{Host: "imap.x", Username: "u", Password: "p", PollIntervalSeconds: 30, Port: "993", MaxMessageMB: -1},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_message_mb") {
		t.Errorf("negative max_message_mb should be rejected, got %v", err)
	}

	// 0 means no limit and is accepted
	cfg.IMAP.MaxMessageMB = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_message_mb 0 should be accepted: %v", err)
	}
}

func TestValidate_AnthropicTokensAndTimeout(t *testing.T) {
	base := func() *Config {
		return &Config{
			HTTP:       HTTPConfig{Port: 8080},
			Database:   DatabaseConfig{Path: "x"},
			Checklists: ChecklistsConfig{Directory: "y"},
			Log:        LogConfig{RotationHours: 24},
			Escalation: EscalationConfig{IntervalMinutes: 15},
			Anthropic:  AnthropicConfig{APIKey: "sk-x", MaxTokens: 2048, TimeoutSec: 30},
		}
	}

	c := base()
	c.Anthropic.MaxTokens = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("max_tokens 0 with API key should be rejected, got %v", err)
	}

	c = base()
	c.Anthropic.TimeoutSec = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
		t.Errorf("timeout_seconds 0 with API key should be rejected, got %v", err)
	}

	// bad values are ignored when no API key is set
	c = base()
	c.Anthropic = AnthropicConfig{MaxTokens: 0, TimeoutSec: 0}
	if err := c.Validate(); err != nil {
		t.Errorf("no API key should not trip anthropic validation: %v", err)
	}
}

func TestValidate_LogRotation(t *testing.T) {
	base := func() *Config {
		return &Config{
			HTTP:       HTTPConfig{Port: 8080},
			Database:   DatabaseConfig{Path: "x"},
			Checklists: ChecklistsConfig{Directory: "y"},
			Log:        LogConfig{MaxAgeDays: 14, RotationHours: 24},
			Escalation: EscalationConfig{IntervalMinutes: 15},
		}
	}

	c := base()
	c.Log.MaxAgeDays = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_age_days") {
		t.Errorf("negative max_age_days should be rejected, got %v", err)
	}

	c = base()
	c.Log.RotationHours = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rotation_hours") {
		t.Errorf("rotation_hours 0 should be rejected, got %v", err)
	}

	// MaxAgeDays 0 is accepted by Validate (the rotator then applies its 7-day default)
	c = base()
	c.Log.MaxAgeDays = 0
	if err := c.Validate(); err != nil {
		t.Errorf("max_age_days 0 should be accepted: %v", err)
	}
}

func TestMaxMessageBytes(t *testing.T) {
	if got := (IMAPConfig{MaxMessageMB: 0}).MaxMessageBytes(); got != 0 {
		t.Errorf("0 MB: got %d, want 0", got)
	}
	if got := (IMAPConfig{MaxMessageMB: 32}).MaxMessageBytes(); got != 32<<20 {
		t.Errorf("32 MB: got %d, want %d", got, 32<<20)
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
	// defaults when unset
	cfg := parseEnv(t, map[string]string{})
	if cfg.SMTP.FromName != "Submission Triage" {
		t.Errorf("smtp.from_name default: got %q", cfg.SMTP.FromName)
	}

	// an explicit SMTP from-name wins
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
