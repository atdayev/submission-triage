package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/database"
	deliveryhttp "github.com/atdayev/submission-triage/internal/delivery/http"
	"github.com/atdayev/submission-triage/internal/delivery/http/handler"
	"github.com/atdayev/submission-triage/internal/delivery/imap"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/email"
	"github.com/atdayev/submission-triage/internal/infrastructure/extractor"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/internal/service"
)

// BuiltApp holds the wired-up dependencies of a constructed app.
type BuiltApp struct {
	DB      *sql.DB
	Service *service.SubmissionsService
	Router  http.Handler
	Poller  *imap.Poller // nil unless IMAP is configured
}

// Build wires up the database, service, router, and optional IMAP poller.
func Build(ctx context.Context, cfg *config.Config, log *logrus.Entry, migrationsDir string) (*BuiltApp, error) {
	resolveDeprecatedConfig(cfg, log)

	db, err := openDB(ctx, cfg, migrationsDir, log)
	if err != nil {
		return nil, err
	}

	repo := repository.NewRepository(db, log)

	checklists, err := checklist.NewYAMLStore(cfg.Checklists.Directory, log)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load checklists: %w", err)
	}

	var llmClient llm.Client
	if cfg.Anthropic.APIKey != "" {
		spend := repository.NewLLMSpendRepository(db)
		llmClient = llm.NewAnthropicClient(cfg.Anthropic, spend, cfg.Retry.Attempts, cfg.Retry.BaseDelay(), log)
	}
	clf := classifier.NewHeuristicLLMClassifier(llmClient)

	// one XOAUTH2 token source serves both IMAP and SMTP (oauth mode only)
	var tokenSource oauth.TokenSource
	if cfg.Mail.OAuthEnabled() {
		ts, err := oauth.NewTokenSource(cfg.Mail.OAuth, authFailureAuditor(repo, log))
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		tokenSource = ts
		log.WithField("provider", cfg.Mail.OAuth.Provider).Info("mail auth: XOAUTH2 enabled")
	}

	sender, err := chooseSender(cfg, tokenSource, log)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	log.WithField("provider", sender.Name()).Info("outbound mail sender selected")

	extractors := buildExtractors()

	svc := service.NewSubmissionsService(service.Dependencies{
		Config:         cfg,
		Repository:     repo,
		EmailSender:    sender,
		Classifier:     clf,
		ChecklistStore: checklists,
		TextExtractors: extractors,
		LLM:            llmClient,
		Log:            log,
	})

	imapAuth := imap.PasswordAuth(cfg.IMAP.Username, cfg.IMAP.Password)
	if tokenSource != nil {
		imapAuth = imap.XOAUTH2Auth(cfg.Mail.OAuth.User, tokenSource)
	}

	var poller *imap.Poller
	if cfg.IMAPConfigured() {
		poller = imap.NewPoller(cfg.IMAP, imapAuth, svc, repo.IngestControl, log)
	} else {
		log.Warn("IMAP not configured; the service will not ingest mail")
	}

	warnStartupGaps(cfg, checklists, sender.Name(), log)

	// a nil *imap.Poller must reach the router as a nil interface, not a typed nil
	var pollStatus handler.PollStatus
	if poller != nil {
		pollStatus = poller
	}
	router := deliveryhttp.NewRouter(db, pollStatus, cfg.PollStaleAfter(), log)

	return &BuiltApp{
		DB:      db,
		Service: svc,
		Router:  router,
		Poller:  poller,
	}, nil
}

// warnStartupGaps reports configurations that pass validation but leave the service
// unable to do its job. Each is legitimate somewhere — a local run, a dry run — so
// none is fatal, but none announces itself at runtime either.
func warnStartupGaps(cfg *config.Config, checklists checklist.Store, senderName string, log *logrus.Entry) {
	if cfg.Digest.Recipient == "" && !anyChecklistDigestRecipient(checklists) {
		log.Error("DIGEST_RECIPIENT is not set; escalation sends no mail of its own, so no status email will reach anyone (mailbox filing still runs)")
	}
	if len(checklists.All()) == 0 {
		log.Error("no checklists loaded; every submission will be treated as an unknown policy type")
	}
	if cfg.Anthropic.APIKey == "" {
		log.Warn("ANTHROPIC_API_KEY is not set; classification is filename/keyword only and no effective date is ever extracted, so bind-window escalation stays dormant")
	}
	if senderName == "log" {
		log.Warn("outbound provider is \"log\"; replies are written to the log and no mail leaves the process")
	}
	if cfg.Reply.RepliesEnabled != nil && !*cfg.Reply.RepliesEnabled {
		log.Warn("REPLIES_ENABLED=false; submissions are ingested and classified but no broker reply is queued, and none is resent when it goes back on")
	}
	if cfg.Digest.IntervalHours <= 0 {
		log.Warn("DIGEST_INTERVAL_HOURS is 0; no daily digest will be sent")
	}
	if cfg.Retention.IntervalHours <= 0 {
		log.Warn("RETENTION_INTERVAL_HOURS is 0; nothing prunes the database and it will grow without bound")
	}
	if cfg.IMAP.MaxMessageMB == 0 {
		log.Warn("IMAP_MAX_MESSAGE_MB is 0; the message size cap is disabled and one large message can exhaust memory")
	}
}

func anyChecklistDigestRecipient(checklists checklist.Store) bool {
	for _, c := range checklists.All() {
		if c.Escalation.DigestRecipient != "" {
			return true
		}
	}
	return false
}

// resolveDeprecatedConfig applies deprecated env aliases onto their replacements
// and warns once, so existing .env files keep working.
func resolveDeprecatedConfig(cfg *config.Config, log *logrus.Entry) {
	if cfg.IMAP.CompleteLabel != "" {
		cfg.IMAP.FolderComplete = cfg.IMAP.CompleteLabel
		log.Warn("config: IMAP_COMPLETE_LABEL is deprecated; use IMAP_FOLDER_COMPLETE")
	}
	if cfg.Digest.LegacyIntervalHours != 0 {
		cfg.Digest.IntervalHours = cfg.Digest.LegacyIntervalHours
		log.Warn("config: ESCALATION_DIGEST_INTERVAL_HOURS is deprecated; use DIGEST_INTERVAL_HOURS")
	}
	if cfg.Digest.LegacyRecipient != "" {
		cfg.Digest.Recipient = cfg.Digest.LegacyRecipient
		log.Warn("config: ESCALATION_DIGEST_RECIPIENT is deprecated; use DIGEST_RECIPIENT")
	}
}

// openDB opens the SQLite database and applies migrations.
func openDB(ctx context.Context, cfg *config.Config, migrationsDir string, log *logrus.Entry) (*sql.DB, error) {
	db, err := database.Open(ctx, cfg.Database.Path, log)
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(ctx, db, migrationsDir, log); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// chooseSender picks the outbound sender from config; "log" never auto-selects.
// A non-nil tokenSource switches SMTP to XOAUTH2.
func chooseSender(cfg *config.Config, tokenSource oauth.TokenSource, log *logrus.Entry) (email.Sender, error) {
	attempts, base := cfg.Retry.Attempts, cfg.Retry.BaseDelay()
	var auth email.AuthMech
	if tokenSource != nil {
		auth = email.XOAUTH2Auth(cfg.Mail.OAuth.User, tokenSource)
	}
	switch strings.ToLower(cfg.Outbound.Provider) {
	case "smtp":
		if !cfg.SMTP.Configured() {
			return nil, errors.New("OUTBOUND_PROVIDER=smtp but SMTP is not configured: set SMTP_HOST and SMTP_FROM_ADDRESS")
		}
		return email.NewSMTPSender(cfg.SMTP, auth, attempts, base, log), nil
	case "log":
		return email.NewLogSender(log), nil
	case "":
		if cfg.SMTP.Configured() {
			return email.NewSMTPSender(cfg.SMTP, auth, attempts, base, log), nil
		}
		return nil, errors.New("no outbound provider configured: set SMTP_*, or OUTBOUND_PROVIDER=log to send nothing")
	default:
		return nil, fmt.Errorf("unknown outbound provider %q (want smtp|log)", cfg.Outbound.Provider)
	}
}

// authFailureAuditor records auth.failed audit events on permanent OAuth token
// failures, carrying the provider and error class but never the token.
func authFailureAuditor(repo *repository.Repository, log *logrus.Entry) oauth.FailureFunc {
	return func(ctx context.Context, provider, class string) {
		entry := &model.AuditEntry{
			EventType: model.EventAuthFailed,
			Payload:   map[string]any{"provider": provider, "class": class},
		}
		if err := repo.Audit.Append(ctx, entry); err != nil {
			log.WithError(err).Warn("auth.failed audit append failed")
		}
	}
}

// buildExtractors maps attachment content types to their text extractor.
func buildExtractors() map[string]service.TextExtractor {
	pdfExt := extractor.NewPDF()
	csvExt := extractor.NewCSV()
	return map[string]service.TextExtractor{
		"application/pdf":   pdfExt,
		"application/x-pdf": pdfExt,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": extractor.NewDOCX(),
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       extractor.NewXLSX(),
		"text/csv":        csvExt,
		"application/csv": csvExt,
	}
}
