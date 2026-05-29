package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Service    ServiceConfig    `yaml:"service"`
	HTTP       HTTPConfig       `yaml:"http"`
	Database   DatabaseConfig   `yaml:"database"`
	Log        LogConfig        `yaml:"log"`
	Postmark   PostmarkConfig   `yaml:"postmark"`
	Anthropic  AnthropicConfig  `yaml:"anthropic"`
	Checklists ChecklistsConfig `yaml:"checklists"`
	Escalation EscalationConfig `yaml:"escalation"`
	Retry      RetryConfig      `yaml:"retry"`
	Reply      ReplyConfig      `yaml:"reply"`
}

type ServiceConfig struct {
	Name string `yaml:"name"`
}

type HTTPConfig struct {
	Port               int `yaml:"port"`
	ReadTimeoutSec     int `yaml:"read_timeout_seconds"`
	WriteTimeoutSec    int `yaml:"write_timeout_seconds"`
	ShutdownTimeoutSec int `yaml:"shutdown_timeout_seconds"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type LogConfig struct {
	Level         string `yaml:"level"`
	Format        string `yaml:"format"`
	Directory     string `yaml:"directory"`
	MaxAgeDays    int    `yaml:"max_age_days"`
	RotationHours int    `yaml:"rotation_hours"`
}

type PostmarkConfig struct {
	ServerToken            string `yaml:"server_token"`
	FromAddress            string `yaml:"from_address"`
	FromName               string `yaml:"from_name"`
	WebhookSecret          string `yaml:"webhook_secret"`
	WebhookSignatureSecret string `yaml:"webhook_signature_secret"`
}

type AnthropicConfig struct {
	APIKey     string `yaml:"api_key"`
	Model      string `yaml:"model"`
	TimeoutSec int    `yaml:"timeout_seconds"`
	MaxTokens  int    `yaml:"max_tokens"`
}

type ChecklistsConfig struct {
	Directory string `yaml:"directory"`
}

type EscalationConfig struct {
	IntervalMinutes     int    `yaml:"interval_minutes"`
	ThresholdHours      int    `yaml:"threshold_hours"`
	AutoCloseAfterHours int    `yaml:"auto_close_after_hours"`
	DigestIntervalHours int    `yaml:"digest_interval_hours"`
	DigestRecipient     string `yaml:"digest_recipient"`
}

type RetryConfig struct {
	Attempts    int `yaml:"attempts"`
	BaseDelayMs int `yaml:"base_delay_ms"`
}

type ReplyConfig struct {
	Workers   int `yaml:"workers"`
	QueueSize int `yaml:"queue_size"`
}

func DefaultPath() string {
	if p := os.Getenv("SUBMISSION_TRIAGE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join("internal", "config", "config.yaml")
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.HTTP.Port <= 0 {
		return errors.New("config: http.port must be > 0")
	}
	if c.Database.Path == "" {
		return errors.New("config: database.path required")
	}
	if c.Checklists.Directory == "" {
		return errors.New("config: checklists.directory required")
	}
	return nil
}

func (h HTTPConfig) ReadTimeout() time.Duration { return time.Duration(h.ReadTimeoutSec) * time.Second }

func (h HTTPConfig) WriteTimeout() time.Duration {
	return time.Duration(h.WriteTimeoutSec) * time.Second
}

func (h HTTPConfig) ShutdownTimeout() time.Duration {
	return time.Duration(h.ShutdownTimeoutSec) * time.Second
}

func (a AnthropicConfig) Timeout() time.Duration { return time.Duration(a.TimeoutSec) * time.Second }

func (e EscalationConfig) Interval() time.Duration {
	return time.Duration(e.IntervalMinutes) * time.Minute
}

func (e EscalationConfig) Threshold() time.Duration {
	return time.Duration(e.ThresholdHours) * time.Hour
}

func (e EscalationConfig) AutoCloseAfter() time.Duration {
	return time.Duration(e.AutoCloseAfterHours) * time.Hour
}

func (e EscalationConfig) DigestInterval() time.Duration {
	return time.Duration(e.DigestIntervalHours) * time.Hour
}

func (r RetryConfig) BaseDelay() time.Duration {
	return time.Duration(r.BaseDelayMs) * time.Millisecond
}

func applyDefaults(c *Config) {
	if c.Service.Name == "" {
		c.Service.Name = "submission-triage"
	}

	if c.HTTP.Port == 0 {
		c.HTTP.Port = 8080
	}
	if c.HTTP.ReadTimeoutSec == 0 {
		c.HTTP.ReadTimeoutSec = 15
	}
	if c.HTTP.WriteTimeoutSec == 0 {
		c.HTTP.WriteTimeoutSec = 30
	}
	if c.HTTP.ShutdownTimeoutSec == 0 {
		c.HTTP.ShutdownTimeoutSec = 10
	}

	if c.Database.Path == "" {
		c.Database.Path = "./data/submission-triage.db"
	}

	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	if c.Log.MaxAgeDays == 0 {
		c.Log.MaxAgeDays = 14
	}
	if c.Log.RotationHours == 0 {
		c.Log.RotationHours = 24
	}

	if c.Anthropic.Model == "" {
		c.Anthropic.Model = "claude-haiku-4-5"
	}
	if c.Anthropic.TimeoutSec == 0 {
		c.Anthropic.TimeoutSec = 30
	}
	if c.Anthropic.MaxTokens == 0 {
		c.Anthropic.MaxTokens = 2048
	}

	if c.Checklists.Directory == "" {
		c.Checklists.Directory = "./checklists"
	}

	if c.Escalation.IntervalMinutes == 0 {
		c.Escalation.IntervalMinutes = 15
	}
	if c.Escalation.ThresholdHours == 0 {
		c.Escalation.ThresholdHours = 72
	}
	if c.Escalation.AutoCloseAfterHours == 0 {
		c.Escalation.AutoCloseAfterHours = 24 * 14
	}
	if c.Escalation.DigestIntervalHours == 0 {
		c.Escalation.DigestIntervalHours = 24
	}

	if c.Retry.Attempts == 0 {
		c.Retry.Attempts = 3
	}
	if c.Retry.BaseDelayMs == 0 {
		c.Retry.BaseDelayMs = 500
	}

	if c.Reply.Workers == 0 {
		c.Reply.Workers = 4
	}
	if c.Reply.QueueSize == 0 {
		c.Reply.QueueSize = 64
	}

	if c.Postmark.FromAddress == "" {
		c.Postmark.FromAddress = "submissions@example.com"
	}
	if c.Postmark.FromName == "" {
		c.Postmark.FromName = "Submission Triage"
	}
}
