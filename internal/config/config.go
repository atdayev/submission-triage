package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

const maxPort = 65535

// Mail authentication modes and OAuth providers.
const (
	AuthModePassword       = "password"
	AuthModeOAuth          = "oauth"
	OAuthProviderGoogle    = "google"
	OAuthProviderMicrosoft = "microsoft"
)

// Config holds all service configuration.
type Config struct {
	Service    ServiceConfig
	HTTP       HTTPConfig
	Database   DatabaseConfig
	Log        LogConfig
	IMAP       IMAPConfig
	SMTP       SMTPConfig
	Mail       MailConfig
	Outbound   OutboundConfig
	Anthropic  AnthropicConfig
	Classifier ClassifierConfig
	Document   DocumentConfig
	Checklists ChecklistsConfig
	Escalation EscalationConfig
	Digest     DigestConfig
	Retry      RetryConfig
	Reply      ReplyConfig
}

// ServiceConfig holds service identity.
type ServiceConfig struct {
	Name string `env:"SERVICE_NAME" envDefault:"submission-triage"`
}

// HTTPConfig holds the HTTP server settings.
type HTTPConfig struct {
	Port                int `env:"HTTP_PORT" envDefault:"8080"`
	ReadTimeoutSec      int `env:"HTTP_READ_TIMEOUT_SECONDS" envDefault:"15"`
	WriteTimeoutSec     int `env:"HTTP_WRITE_TIMEOUT_SECONDS" envDefault:"30"`
	ShutdownTimeoutSec  int `env:"HTTP_SHUTDOWN_TIMEOUT_SECONDS" envDefault:"10"`
	PollStaleMultiplier int `env:"HEALTH_POLL_STALE_MULTIPLIER" envDefault:"3"`
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

	// FileByStatus files each processed message into a per-status folder (MOVE).
	FileByStatus        bool   `env:"IMAP_FILE_BY_STATUS" envDefault:"true"`
	FolderPrefix        string `env:"IMAP_FOLDER_PREFIX" envDefault:"Triage"`
	FolderComplete      string `env:"IMAP_FOLDER_COMPLETE" envDefault:"Ready for Underwriting"`
	FolderAwaiting      string `env:"IMAP_FOLDER_AWAITING" envDefault:"Waiting on Broker"`
	FolderEscalated     string `env:"IMAP_FOLDER_ESCALATED" envDefault:"Escalated"`
	FolderUnknownPolicy string `env:"IMAP_FOLDER_UNKNOWN_POLICY" envDefault:"Unknown Policy"`
	// CompleteLabel is a deprecated alias for FolderComplete (no default).
	CompleteLabel string `env:"IMAP_COMPLETE_LABEL"`
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

// MailConfig selects how the IMAP and SMTP clients authenticate. Password mode
// is the default and unchanged; oauth mode uses XOAUTH2 with a shared token source.
type MailConfig struct {
	AuthMode string `env:"MAIL_AUTH_MODE" envDefault:"password"`
	OAuth    OAuthConfig
}

// OAuthConfig holds the XOAUTH2 provider credentials shared by IMAP and SMTP.
type OAuthConfig struct {
	Provider     string `env:"MAIL_OAUTH_PROVIDER"`
	ClientID     string `env:"MAIL_OAUTH_CLIENT_ID"`
	ClientSecret string `env:"MAIL_OAUTH_CLIENT_SECRET"`
	TenantID     string `env:"MAIL_OAUTH_TENANT_ID"`     // microsoft only
	RefreshToken string `env:"MAIL_OAUTH_REFRESH_TOKEN"` // google only
	User         string `env:"MAIL_OAUTH_USER"`          // mailbox address, the XOAUTH2 user= field
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
	// DailyCapUSD bounds estimated LLM spend per UTC day; 0 disables the cap.
	DailyCapUSD float64 `env:"LLM_DAILY_USD_CAP" envDefault:"2.00"`
}

// ClassifierConfig holds classification thresholds.
type ClassifierConfig struct {
	// ConfidenceFloor is the minimum confidence a classified document needs to
	// satisfy a checklist item. Default 0.80 keeps both heuristic paths (0.95
	// filename, 0.85 content) satisfying items and routes only lower-confidence
	// LLM results to review.
	ConfidenceFloor float64 `env:"CLASSIFIER_CONFIDENCE_FLOOR" envDefault:"0.80"`
}

// DocumentConfig holds document-quality thresholds.
type DocumentConfig struct {
	// UnreadableMinSizeBytes is the size at/above which a PDF yielding almost no
	// text is treated as a scan (not a genuinely sparse small document).
	UnreadableMinSizeBytes int64 `env:"UNREADABLE_MIN_SIZE_BYTES" envDefault:"51200"`
	// UnreadableMinChars is the extracted-character count below which such a PDF
	// is flagged unreadable.
	UnreadableMinChars int `env:"UNREADABLE_MIN_CHARS" envDefault:"100"`
}

// ChecklistsConfig holds the checklists directory path.
type ChecklistsConfig struct {
	Directory string `env:"CHECKLISTS_DIR" envDefault:"./checklists"`
}

// EscalationConfig holds escalation timing settings.
type EscalationConfig struct {
	IntervalMinutes     int `env:"ESCALATION_INTERVAL_MINUTES" envDefault:"15"`
	ThresholdHours      int `env:"ESCALATION_THRESHOLD_HOURS" envDefault:"72"`
	AutoCloseAfterHours int `env:"ESCALATION_AUTO_CLOSE_AFTER_HOURS" envDefault:"336"`
}

// DigestConfig holds the daily status-digest settings.
type DigestConfig struct {
	IntervalHours int    `env:"DIGEST_INTERVAL_HOURS" envDefault:"24"`
	Recipient     string `env:"DIGEST_RECIPIENT"`
	MaxRows       int    `env:"DIGEST_MAX_ROWS" envDefault:"500"`
	// deprecated aliases (no defaults); resolved onto the fields above at startup
	LegacyIntervalHours int    `env:"ESCALATION_DIGEST_INTERVAL_HOURS"`
	LegacyRecipient     string `env:"ESCALATION_DIGEST_RECIPIENT"`
}

// RetryConfig holds retry attempt and backoff settings.
type RetryConfig struct {
	Attempts    int `env:"RETRY_ATTEMPTS" envDefault:"3"`
	BaseDelayMs int `env:"RETRY_BASE_DELAY_MS" envDefault:"500"`
}

// ReplyConfig holds the reply worker pool and coalescing settings.
type ReplyConfig struct {
	Workers   int `env:"REPLY_WORKERS" envDefault:"4"`
	QueueSize int `env:"REPLY_QUEUE_SIZE" envDefault:"64"`
	// CoalesceWindowSeconds spaces replies per submission: a reply waits until
	// this long after the previous one was sent, so rapid follow-ups collapse
	// into one. 0 sends every reply immediately (no coalescing).
	CoalesceWindowSeconds int `env:"REPLY_COALESCE_WINDOW_SECONDS" envDefault:"120"`
	// FlushIntervalSeconds is how often the outbox sweeper runs, sending replies
	// whose coalesce window has elapsed and retrying failed sends.
	FlushIntervalSeconds int `env:"REPLY_FLUSH_INTERVAL_SECONDS" envDefault:"30"`
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
	if err := c.Mail.Validate(); err != nil {
		return err
	}
	if c.IMAPConfigured() {
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

// PollStaleAfter is how long since the last successful poll before /health
// reports the IMAP poller stale.
func (c *Config) PollStaleAfter() time.Duration {
	m := max(c.HTTP.PollStaleMultiplier, 1) // never zero, which would report every poll stale
	return c.IMAP.PollInterval() * time.Duration(m)
}

func (h HTTPConfig) ReadTimeout() time.Duration { return time.Duration(h.ReadTimeoutSec) * time.Second }

func (h HTTPConfig) WriteTimeout() time.Duration {
	return time.Duration(h.WriteTimeoutSec) * time.Second
}

func (h HTTPConfig) ShutdownTimeout() time.Duration {
	return time.Duration(h.ShutdownTimeoutSec) * time.Second
}

// Timeout returns the per-call Anthropic timeout.
func (a AnthropicConfig) Timeout() time.Duration { return time.Duration(a.TimeoutSec) * time.Second }

// Configured reports whether enough is set for password-mode IMAP.
func (i IMAPConfig) Configured() bool {
	return i.Host != "" && i.Username != "" && i.Password != ""
}

// IMAPConfigured reports whether the IMAP poller should start. Oauth mode takes
// the mailbox identity from MAIL_OAUTH_USER, so it needs only the host; password
// mode still needs username and password.
func (c *Config) IMAPConfigured() bool {
	if c.Mail.OAuthEnabled() {
		return c.IMAP.Host != ""
	}
	return c.IMAP.Configured()
}

// OAuthEnabled reports whether XOAUTH2 auth is selected.
func (m MailConfig) OAuthEnabled() bool { return strings.EqualFold(m.AuthMode, AuthModeOAuth) }

// Validate reports the first mail-auth configuration error, if any.
func (m MailConfig) Validate() error {
	switch strings.ToLower(m.AuthMode) {
	case "", AuthModePassword:
		return nil
	case AuthModeOAuth:
		return m.OAuth.validate()
	default:
		return fmt.Errorf("config: MAIL_AUTH_MODE %q invalid (want password|oauth)", m.AuthMode)
	}
}

func (o OAuthConfig) validate() error {
	if o.Provider == "" || o.ClientID == "" || o.ClientSecret == "" || o.User == "" {
		return errors.New("config: oauth requires MAIL_OAUTH_PROVIDER, MAIL_OAUTH_CLIENT_ID, MAIL_OAUTH_CLIENT_SECRET and MAIL_OAUTH_USER")
	}
	switch strings.ToLower(o.Provider) {
	case OAuthProviderMicrosoft:
		if o.TenantID == "" {
			return errors.New("config: MAIL_OAUTH_PROVIDER=microsoft requires MAIL_OAUTH_TENANT_ID")
		}
	case OAuthProviderGoogle:
		if o.RefreshToken == "" {
			return errors.New("config: MAIL_OAUTH_PROVIDER=google requires MAIL_OAUTH_REFRESH_TOKEN")
		}
	default:
		return fmt.Errorf("config: MAIL_OAUTH_PROVIDER %q invalid (want google|microsoft)", o.Provider)
	}
	return nil
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

// Interval returns how often the daily status digest is sent.
func (d DigestConfig) Interval() time.Duration {
	return time.Duration(d.IntervalHours) * time.Hour
}

func (r RetryConfig) BaseDelay() time.Duration {
	return time.Duration(r.BaseDelayMs) * time.Millisecond
}

// CoalesceWindow returns the per-submission reply spacing.
func (r ReplyConfig) CoalesceWindow() time.Duration {
	return time.Duration(r.CoalesceWindowSeconds) * time.Second
}

// FlushInterval returns how often the outbox sweeper runs.
func (r ReplyConfig) FlushInterval() time.Duration {
	return time.Duration(r.FlushIntervalSeconds) * time.Second
}
