package http

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/delivery/http/handler"
)

func NewRouter(db *sql.DB, log *logrus.Entry) http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID(log))
	r.Use(withRecovery())

	health := handler.NewHealthHandler(db, log)
	r.Get("/health", health.Handle)

	return r
}
