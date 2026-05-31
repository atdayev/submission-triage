package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Service    ServiceConfig
	HTTP       HTTPConfig
	Database   DatabaseConfig
	Log        LogConfig
	Postmark   PostmarkConfig
	IMAP       IMAPConfig
	SMTP       SMTPConfig
	Outbound   OutboundConfig
	Anthropic  AnthropicConfig
	Checklists ChecklistsConfig
	Escalation EscalationConfig
	Retry      RetryConfig
	Reply      ReplyConfig
}

type ServiceConfig struct {
	Name string `env:"SERVICE_NAME" envDefault:"submission-triage"`
}

type HTTPConfig struct {
	Port               int `env:"HTTP_PORT" envDefault:"8080"`
	ReadTimeoutSec     int `env:"HTTP_READ_TIMEOUT_SECONDS" envDefault:"15"`
	WriteTimeoutSec    int `env:"HTTP_WRITE_TIMEOUT_SECONDS" envDefault:"30"`
	ShutdownTimeoutSec int `env:"HTTP_SHUTDOWN_TIMEOUT_SECONDS" envDefault:"10"`
}

type DatabaseConfig struct {
	Path string `env:"DB_PATH" envDefault:"./data/submission-triage.db"`
}

type LogConfig struct {
	Level         string `env:"LOG_LEVEL" envDefault:"info"`
	Format        string `env:"LOG_FORMAT" envDefault:"json"`
	Directory     string `env:"LOG_DIR"`
	MaxAgeDays    int    `env:"LOG_MAX_AGE_DAYS" envDefault:"14"`
	RotationHours int    `env:"LOG_ROTATION_HOURS" envDefault:"24"`
}

type PostmarkConfig struct {
	ServerToken            string `env:"POSTMARK_SERVER_TOKEN"`
	FromAddress            string `env:"POSTMARK_FROM_ADDRESS" envDefault:"submissions@example.com"`
	FromName               string `env:"POSTMARK_FROM_NAME" envDefault:"Submission Triage"`
	WebhookSecret          string `env:"POSTMARK_WEBHOOK_SECRET"`
	WebhookSignatureSecret string `env:"POSTMARK_WEBHOOK_SIGNATURE_SECRET"`
}

// IMAPConfig drives the optional inbound poller. It activates only when host,
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

// SMTPConfig drives the optional SMTP outbound sender (Gmail App Password,
// Microsoft 365, or any SMTP server).
type SMTPConfig struct {
	Host        string `env:"SMTP_HOST"`
	Port        string `env:"SMTP_PORT" envDefault:"587"`
	Username    string `env:"SMTP_USERNAME"`
	Password    string `env:"SMTP_PASSWORD"`
	FromAddress string `env:"SMTP_FROM_ADDRESS"`
	FromName    string `env:"SMTP_FROM_NAME"`
}

// OutboundConfig selects the reply channel. Provider is "postmark", "smtp",
// "log", or "" for auto (smtp if configured, else postmark, else log).
type OutboundConfig struct {
	Provider string `env:"OUTBOUND_PROVIDER"`
}

type AnthropicConfig struct {
	APIKey     string `env:"ANTHROPIC_API_KEY"`
	Model      string `env:"ANTHROPIC_MODEL" envDefault:"claude-haiku-4-5"`
	TimeoutSec int    `env:"ANTHROPIC_TIMEOUT_SECONDS" envDefault:"30"`
	MaxTokens  int    `env:"ANTHROPIC_MAX_TOKENS" envDefault:"2048"`
}

type ChecklistsConfig struct {
	Directory string `env:"CHECKLISTS_DIR" envDefault:"./checklists"`
}

type EscalationConfig struct {
	IntervalMinutes     int    `env:"ESCALATION_INTERVAL_MINUTES" envDefault:"15"`
	ThresholdHours      int    `env:"ESCALATION_THRESHOLD_HOURS" envDefault:"72"`
	AutoCloseAfterHours int    `env:"ESCALATION_AUTO_CLOSE_AFTER_HOURS" envDefault:"336"`
	DigestIntervalHours int    `env:"ESCALATION_DIGEST_INTERVAL_HOURS" envDefault:"24"`
	DigestRecipient     string `env:"ESCALATION_DIGEST_RECIPIENT"`
}

type RetryConfig struct {
	Attempts    int `env:"RETRY_ATTEMPTS" envDefault:"3"`
	BaseDelayMs int `env:"RETRY_BASE_DELAY_MS" envDefault:"500"`
}

type ReplyConfig struct {
	Workers   int `env:"REPLY_WORKERS" envDefault:"4"`
	QueueSize int `env:"REPLY_QUEUE_SIZE" envDefault:"64"`
}

// Load reads configuration from the process environment (populated from .env at
// startup). Defaults live in the envDefault struct tags.
func Load() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse config from environment: %w", err)
	}
	applyDerivedDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDerivedDefaults fills values that depend on another field, which a
// static envDefault tag cannot express.
func applyDerivedDefaults(c *Config) {
	if c.SMTP.FromName == "" {
		c.SMTP.FromName = c.Postmark.FromName
	}
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

// Configured reports whether enough is set to start the IMAP poller.
func (i IMAPConfig) Configured() bool {
	return i.Host != "" && i.Username != "" && i.Password != ""
}

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
