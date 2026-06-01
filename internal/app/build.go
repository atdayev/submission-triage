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
	"github.com/atdayev/submission-triage/internal/delivery/imap"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/email"
	"github.com/atdayev/submission-triage/internal/infrastructure/extractor"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/telemetry"
)

type BuiltApp struct {
	DB      *sql.DB
	Service *service.SubmissionsService
	Router  http.Handler
	Poller  *imap.Poller // nil unless IMAP is configured
}

func Build(ctx context.Context, cfg *config.Config, log *logrus.Entry, migrationsDir string, metrics *telemetry.Metrics) (*BuiltApp, error) {
	db, err := database.Open(ctx, cfg.Database.Path, log)
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(ctx, db, migrationsDir, log); err != nil {
		_ = db.Close()
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
		llmClient = llm.NewAnthropicClient(cfg.Anthropic, cfg.Retry.Attempts, cfg.Retry.BaseDelay(), log)
	}
	clf := classifier.NewHeuristicLLMClassifier(llmClient)

	sender, err := chooseSender(cfg, log)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	log.WithField("provider", sender.Name()).Info("outbound mail sender selected")

	pdfExt := extractor.NewPDF()
	csvExt := extractor.NewCSV()
	extractors := map[string]service.TextExtractor{
		"application/pdf":   pdfExt,
		"application/x-pdf": pdfExt,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": extractor.NewDOCX(),
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       extractor.NewXLSX(),
		"text/csv":        csvExt,
		"application/csv": csvExt,
	}

	svc := service.NewSubmissionsService(service.Dependencies{
		Config:         cfg,
		Repository:     repo,
		EmailSender:    sender,
		Classifier:     clf,
		ChecklistStore: checklists,
		TextExtractors: extractors,
		LLM:            llmClient,
		Metrics:        metrics,
		Log:            log,
	})

	router := deliveryhttp.NewRouter(db, log)

	var poller *imap.Poller
	if cfg.IMAP.Configured() {
		poller = imap.NewPoller(cfg.IMAP, svc, log)
	} else {
		log.Warn("IMAP not configured; the service will not ingest mail")
	}

	return &BuiltApp{
		DB:      db,
		Service: svc,
		Router:  router,
		Poller:  poller,
	}, nil
}

// chooseSender picks the outbound channel: an explicit outbound.provider wins;
// empty auto-selects SMTP. "log" never auto-selects (it sends nothing), so a
// missing SMTP config is a startup error, not a silent no-op.
func chooseSender(cfg *config.Config, log *logrus.Entry) (email.Sender, error) {
	attempts, base := cfg.Retry.Attempts, cfg.Retry.BaseDelay()
	switch strings.ToLower(cfg.Outbound.Provider) {
	case "smtp":
		return email.NewSMTPSender(cfg.SMTP, attempts, base, log), nil
	case "log":
		return email.NewLogSender(log), nil
	case "":
		if cfg.SMTP.Configured() {
			return email.NewSMTPSender(cfg.SMTP, attempts, base, log), nil
		}
		return nil, errors.New("no outbound provider configured: set SMTP_*, or OUTBOUND_PROVIDER=log to send nothing")
	default:
		return nil, fmt.Errorf("unknown outbound provider %q (want smtp|log)", cfg.Outbound.Provider)
	}
}
