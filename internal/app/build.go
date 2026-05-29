package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/database"
	deliveryhttp "github.com/atdayev/submission-triage/internal/delivery/http"
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

	var sender email.Sender
	if cfg.Postmark.ServerToken != "" {
		sender = email.NewPostmarkSender(cfg.Postmark, cfg.Retry.Attempts, cfg.Retry.BaseDelay(), log)
	} else {
		log.Warn("postmark server token not set; falling back to log-only sender")
		sender = email.NewLogSender(log)
	}

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

	router := deliveryhttp.NewRouter(cfg, svc, db, log)

	return &BuiltApp{
		DB:      db,
		Service: svc,
		Router:  router,
	}, nil
}
