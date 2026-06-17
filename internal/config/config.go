package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/caarlos0/env/v11"
)

const maxPort = 65535

// Config holds all service configuration.
type Config struct {
	Service    ServiceConfig
	HTTP       HTTPConfig
	Database   DatabaseConfig
	Log        LogConfig
	IMAP       IMAPConfig
	SMTP       SMTPConfig
	Outbound   OutboundConfig
	Anthropic  AnthropicConfig
	Checklists ChecklistsConfig
	Escalation EscalationConfig
	Retry      RetryConfig
	Reply      ReplyConfig
}

// ServiceConfig holds service identity.
type ServiceConfig struct {
	Name string `env:"SERVICE_NAME" envDefault:"submission-triage"`
}

// HTTPConfig holds the HTTP server settings.
type HTTPConfig struct {
	Port               int `env:"HTTP_PORT" envDefault:"8080"`
	ReadTimeoutSec     int `env:"HTTP_READ_TIMEOUT_SECONDS" envDefault:"15"`
	WriteTimeoutSec    int `env:"HTTP_WRITE_TIMEOUT_SECONDS" envDefault:"30"`
	ShutdownTimeoutSec int `env:"HTTP_SHUTDOWN_TIMEOUT_SECONDS" envDefault:"10"`
}

// DatabaseConfig holds the SQLite database path.
type DatabaseConfig struct {
	Path string `env:"DB_PATH" envDefault:"./data/submission-triage.db"`
}

// LogConfig holds logging and rotation settings.
type LogConfig struct {
	Level         string `env:"LOG_LEVEL" envDefault:"info"`
	Format        string `env:"LOG_FORMAT" envDefault:"json"`
	Directory     string `env:"LOG_DIR"`
	MaxAgeDays    int    `env:"LOG_MAX_AGE_DAYS" envDefault:"14"`
	RotationHours int    `env:"LOG_ROTATION_HOURS" envDefault:"24"`
}

// IMAPConfig drives the optional inbound poller; active only when host,
// username, and password are all set.
type IMAPConfig struct {
	Host                string `env:"IMAP_HOST"`
	Port                string `env:"IMAP_PORT" envDefault:"993"`
	Username            string `env:"IMAP_USERNAME"`
	Password            string `env:"IMAP_PASSWORD"`
	Mailbox             string `env:"IMAP_MAILBOX" envDefault:"INBOX"`
	PollIntervalSeconds int    `env:"IMAP_POLL_INTERVAL_SECONDS" envDefault:"30"`
	MaxMessageMB        int    `env:"IMAP_MAX_MESSAGE_MB" envDefault:"32"`
}

// SMTPConfig drives the optional SMTP outbound sender.
type SMTPConfig struct {
	Host        string `env:"SMTP_HOST"`
	Port        string `env:"SMTP_PORT" envDefault:"587"`
	Username    string `env:"SMTP_USERNAME"`
	Password    string `env:"SMTP_PASSWORD"`
	FromAddress string `env:"SMTP_FROM_ADDRESS"`
	FromName    string `env:"SMTP_FROM_NAME" envDefault:"Submission Triage"`
}

// OutboundConfig selects the reply channel: "smtp", "log", or "" for auto.
type OutboundConfig struct {
	Provider string `env:"OUTBOUND_PROVIDER"`
}

// AnthropicConfig holds the Anthropic API client settings.
type AnthropicConfig struct {
	APIKey     string `env:"ANTHROPIC_API_KEY"`
	Model      string `env:"ANTHROPIC_MODEL" envDefault:"claude-haiku-4-5"`
	TimeoutSec int    `env:"ANTHROPIC_TIMEOUT_SECONDS" envDefault:"30"`
	MaxTokens  int    `env:"ANTHROPIC_MAX_TOKENS" envDefault:"2048"`
}

// ChecklistsConfig holds the checklists directory path.
type ChecklistsConfig struct {
	Directory string `env:"CHECKLISTS_DIR" envDefault:"./checklists"`
}

// EscalationConfig holds escalation timing and digest settings.
type EscalationConfig struct {
	IntervalMinutes     int    `env:"ESCALATION_INTERVAL_MINUTES" envDefault:"15"`
	ThresholdHours      int    `env:"ESCALATION_THRESHOLD_HOURS" envDefault:"72"`
	AutoCloseAfterHours int    `env:"ESCALATION_AUTO_CLOSE_AFTER_HOURS" envDefault:"336"`
	DigestIntervalHours int    `env:"ESCALATION_DIGEST_INTERVAL_HOURS" envDefault:"24"`
	DigestRecipient     string `env:"ESCALATION_DIGEST_RECIPIENT"`
}

// RetryConfig holds retry attempt and backoff settings.
type RetryConfig struct {
	Attempts    int `env:"RETRY_ATTEMPTS" envDefault:"3"`
	BaseDelayMs int `env:"RETRY_BASE_DELAY_MS" envDefault:"500"`
}

// ReplyConfig holds the reply worker pool settings.
type ReplyConfig struct {
	Workers   int `env:"REPLY_WORKERS" envDefault:"4"`
	QueueSize int `env:"REPLY_QUEUE_SIZE" envDefault:"64"`
}

// Load reads configuration from the process environment; defaults live in the
// envDefault struct tags.
func Load() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse config from environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate reports the first configuration error, if any.
func (c *Config) Validate() error {
	if c.HTTP.Port < 1 || c.HTTP.Port > maxPort {
		return errors.New("config: http.port must be in 1..65535")
	}
	if c.Database.Path == "" {
		return errors.New("config: database.path required")
	}
	if c.Checklists.Directory == "" {
		return errors.New("config: checklists.directory required")
	}
	if c.Log.MaxAgeDays < 0 {
		return errors.New("config: log.max_age_days must be >= 0")
	}
	if c.Log.RotationHours <= 0 {
		return errors.New("config: log.rotation_hours must be > 0")
	}
	if c.Escalation.IntervalMinutes <= 0 {
		return errors.New("config: escalation.interval_minutes must be > 0")
	}
	if c.IMAP.Configured() {
		if c.IMAP.PollIntervalSeconds <= 0 {
			return errors.New("config: imap.poll_interval_seconds must be > 0")
		}
		if c.IMAP.MaxMessageMB < 0 {
			return errors.New("config: imap.max_message_mb must be >= 0")
		}
		if err := validatePort("imap.port", c.IMAP.Port); err != nil {
			return err
		}
	}
	if c.SMTP.Configured() {
		if err := validatePort("smtp.port", c.SMTP.Port); err != nil {
			return err
		}
	}
	if c.Anthropic.APIKey != "" {
		if c.Anthropic.MaxTokens <= 0 {
			return errors.New("config: anthropic.max_tokens must be > 0")
		}
		if c.Anthropic.TimeoutSec <= 0 {
			return errors.New("config: anthropic.timeout_seconds must be > 0")
		}
	}
	return nil
}

// validatePort parses raw and checks it falls in 1..65535.
func validatePort(name, raw string) error {
	p, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("config: %s %q is not a number: %w", name, raw, err)
	}
	if p < 1 || p > maxPort {
		return fmt.Errorf("config: %s must be in 1..65535", name)
	}
	return nil
}

// ReadTimeout returns the HTTP read timeout.
func (h HTTPConfig) ReadTimeout() time.Duration { return time.Duration(h.ReadTimeoutSec) * time.Second }

// WriteTimeout returns the HTTP write timeout.
func (h HTTPConfig) WriteTimeout() time.Duration {
	return time.Duration(h.WriteTimeoutSec) * time.Second
}

// ShutdownTimeout returns the graceful-shutdown timeout.
func (h HTTPConfig) ShutdownTimeout() time.Duration {
	return time.Duration(h.ShutdownTimeoutSec) * time.Second
}

// Timeout returns the per-call Anthropic timeout.
func (a AnthropicConfig) Timeout() time.Duration { return time.Duration(a.TimeoutSec) * time.Second }

// Configured reports whether enough is set to start the IMAP poller.
func (i IMAPConfig) Configured() bool {
	return i.Host != "" && i.Username != "" && i.Password != ""
}

// PollInterval returns the inbox poll interval.
func (i IMAPConfig) PollInterval() time.Duration {
	return time.Duration(i.PollIntervalSeconds) * time.Second
}

// MaxMessageBytes is the size above which a message is skipped instead of
// pulled into memory. Zero means no limit.
func (i IMAPConfig) MaxMessageBytes() int64 {
	return int64(i.MaxMessageMB) << 20
}

// Configured reports whether enough is set to use the SMTP sender.
func (s SMTPConfig) Configured() bool {
	return s.Host != "" && s.FromAddress != ""
}

// Interval returns how often the escalation worker runs.
func (e EscalationConfig) Interval() time.Duration {
	return time.Duration(e.IntervalMinutes) * time.Minute
}

// Threshold returns the quiet time before a case escalates.
func (e EscalationConfig) Threshold() time.Duration {
	return time.Duration(e.ThresholdHours) * time.Hour
}

// AutoCloseAfter returns the quiet time before a completed case auto-closes.
func (e EscalationConfig) AutoCloseAfter() time.Duration {
	return time.Duration(e.AutoCloseAfterHours) * time.Hour
}

// DigestInterval returns how often the escalation digest is sent.
func (e EscalationConfig) DigestInterval() time.Duration {
	return time.Duration(e.DigestIntervalHours) * time.Hour
}

// BaseDelay returns the base retry backoff delay.
func (r RetryConfig) BaseDelay() time.Duration {
	return time.Duration(r.BaseDelayMs) * time.Millisecond
}
