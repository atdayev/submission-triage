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

// HealthHandler reports service health, including database reachability.
type HealthHandler struct {
	db  *sql.DB
	log *logrus.Entry
}

// NewHealthHandler returns a HealthHandler.
func NewHealthHandler(db *sql.DB, log *logrus.Entry) *HealthHandler {
	return &HealthHandler{db: db, log: log}
}

// Handle responds 200 when the database pings, else 503.
func (h *HealthHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pingCtx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()

	if err := h.db.PingContext(pingCtx); err != nil {
		h.log.WithError(err).Error("health db ping failed")
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded",
			"db":     "down",
		})
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "ok",
		"db":     "ok",
	})
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
