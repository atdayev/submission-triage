package http

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/delivery/http/handler"
)

// NewRouter builds the HTTP router with request-ID and recovery middleware.
// poll may be nil when IMAP is not configured.
func NewRouter(db *sql.DB, poll handler.PollStatus, staleAfter time.Duration, log *logrus.Entry) http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID(log))
	r.Use(withRecovery())

	health := handler.NewHealthHandler(db, poll, staleAfter, log)
	r.Get("/health", health.Handle)

	return r
}
