package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/atdayev/submission-triage/pkg/apperror"
	"github.com/atdayev/submission-triage/pkg/logger"
)

const MaxInboundBodyBytes = 32 << 20

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

func WriteJSONError(w http.ResponseWriter, r *http.Request, status int, resp *apperror.ErrorResponse) {
	if resp == nil {
		resp = apperror.NewErrorResponse(apperror.CodeInternal, "unknown error")
	}
	rid := logger.RequestIDFromContext(r.Context())
	if rid != "" {
		resp = resp.WithRequestID(rid)
	}
	WriteJSON(w, r, status, resp)
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxInboundBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return fmt.Errorf("request body exceeds %d bytes", MaxInboundBodyBytes)
		case errors.Is(err, io.EOF):
			return fmt.Errorf("empty body")
		default:
			return fmt.Errorf("decode body: %w", err)
		}
	}
	if dec.More() {
		return fmt.Errorf("decode body: unexpected trailing data")
	}
	return nil
}
