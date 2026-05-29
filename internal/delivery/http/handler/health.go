package handler

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/pkg/utils"
)

const (
	dbPingTimeout       = 2 * time.Second
	postmarkPingTimeout = 3 * time.Second
	postmarkPingURL     = "https://api.postmarkapp.com/server"
)

type HealthHandler struct {
	db   *sql.DB
	cfg  config.PostmarkConfig
	http *http.Client
	log  *logrus.Entry
}

func NewHealthHandler(db *sql.DB, cfg config.PostmarkConfig, log *logrus.Entry) *HealthHandler {
	return &HealthHandler{
		db:   db,
		cfg:  cfg,
		http: &http.Client{Timeout: postmarkPingTimeout},
		log:  log,
	}
}

func (h *HealthHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var (
		wg                       sync.WaitGroup
		dbStatus, postmarkStatus string
		dbOK, postmarkOK         bool
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		pingCtx, cancel := context.WithTimeout(ctx, dbPingTimeout)
		defer cancel()
		if err := h.db.PingContext(pingCtx); err != nil {
			dbStatus = "down: " + err.Error()
			return
		}
		dbStatus = "ok"
		dbOK = true
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if h.cfg.ServerToken == "" {
			postmarkStatus = "not_configured"
			postmarkOK = true
			return
		}
		pingCtx, cancel := context.WithTimeout(ctx, postmarkPingTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, postmarkPingURL, nil)
		if err != nil {
			postmarkStatus = "down: " + err.Error()
			return
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Postmark-Server-Token", h.cfg.ServerToken)
		resp, err := h.http.Do(req)
		if err != nil {
			postmarkStatus = "down: " + err.Error()
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			postmarkStatus = "down: status " + http.StatusText(resp.StatusCode)
			return
		}
		postmarkStatus = "ok"
		postmarkOK = true
	}()

	wg.Wait()

	status := http.StatusOK
	overall := "ok"
	if !dbOK || !postmarkOK {
		status = http.StatusServiceUnavailable
		overall = "degraded"
	}
	utils.WriteJSON(w, r, status, map[string]any{
		"status":   overall,
		"db":       dbStatus,
		"postmark": postmarkStatus,
	})
}
