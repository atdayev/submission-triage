package utils

import (
	"encoding/json"
	"net/http"

	"github.com/atdayev/submission-triage/pkg/logger"
)

func WriteJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
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
