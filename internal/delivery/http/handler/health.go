package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

const dbPingTimeout = 2 * time.Second

// PollStatus reports IMAP poll freshness; nil when IMAP is not configured.
type PollStatus interface {
	LastSuccessfulPoll() time.Time
	Configured() bool
}

// HealthHandler reports service health: database reachability and poll freshness.
type HealthHandler struct {
	db         *sql.DB
	poll       PollStatus // nil when IMAP is not configured
	staleAfter time.Duration
	log        *logrus.Entry
}

// NewHealthHandler returns a HealthHandler. poll may be nil.
func NewHealthHandler(db *sql.DB, poll PollStatus, staleAfter time.Duration, log *logrus.Entry) *HealthHandler {
	return &HealthHandler{db: db, poll: poll, staleAfter: staleAfter, log: log}
}

// Handle responds 200 when the database pings and the poller is fresh, else 503.
func (h *HealthHandler) Handle(w http.ResponseWriter, r *http.Request) {
	pingCtx, cancel := context.WithTimeout(r.Context(), dbPingTimeout)
	defer cancel()

	status := http.StatusOK
	body := map[string]any{"status": "ok", "db": "ok"}

	if err := h.db.PingContext(pingCtx); err != nil {
		h.log.WithError(err).Error("health db ping failed")
		body["db"] = "down"
		status = http.StatusServiceUnavailable
	}

	h.reportPoll(body, &status)

	if status != http.StatusOK {
		body["status"] = "degraded"
	}
	writeJSON(w, r, status, body)
}

// reportPoll adds IMAP freshness to body, degrading status when the poll is stale.
func (h *HealthHandler) reportPoll(body map[string]any, status *int) {
	if h.poll == nil || !h.poll.Configured() {
		body["imap"] = "not_configured"
		return
	}
	last := h.poll.LastSuccessfulPoll()
	body["last_poll_at"] = last.UTC().Format(time.RFC3339)
	if time.Since(last) > h.staleAfter {
		body["imap"] = "stale"
		*status = http.StatusServiceUnavailable
		return
	}
	body["imap"] = "ok"
}

// writeJSON writes payload as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.GetLoggerFromContext(r.Context()).
			WithError(err).
			Error("encode json response failed")
	}
}
