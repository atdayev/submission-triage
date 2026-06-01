package handler

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/utils"
)

const dbPingTimeout = 2 * time.Second

type HealthHandler struct {
	db  *sql.DB
	log *logrus.Entry
}

func NewHealthHandler(db *sql.DB, log *logrus.Entry) *HealthHandler {
	return &HealthHandler{db: db, log: log}
}

func (h *HealthHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pingCtx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()

	if err := h.db.PingContext(pingCtx); err != nil {
		utils.WriteJSON(w, r, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded",
			"db":     "down: " + err.Error(),
		})
		return
	}

	utils.WriteJSON(w, r, http.StatusOK, map[string]any{
		"status": "ok",
		"db":     "ok",
	})
}
