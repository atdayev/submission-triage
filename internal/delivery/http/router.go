package http

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/delivery/http/handler"
	"github.com/atdayev/submission-triage/internal/service"
)

func NewRouter(cfg *config.Config, svc *service.SubmissionsService, db *sql.DB, log *logrus.Entry) http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID(log))
	r.Use(withRecovery())

	webhook := handler.NewWebhookHandler(svc, cfg.Postmark, log)
	health := handler.NewHealthHandler(db, cfg.Postmark, log)

	r.Get("/health", health.Handle)

	// mount only with a secret/signature set; no unauthenticated public ingest
	if cfg.Postmark.WebhookSecret != "" || cfg.Postmark.WebhookSignatureSecret != "" {
		r.Post("/webhooks/postmark", webhook.Handle)
	} else {
		log.Warn("postmark webhook secret not set; /webhooks/postmark route disabled")
	}

	return r
}
