package http

import (
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/pkg/logger"
)

const requestIDHeader = "X-Request-ID"

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(s int) {
	r.status = s
	r.ResponseWriter.WriteHeader(s)
}

func withRequestID(log *logrus.Entry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(requestIDHeader)
			if _, err := uuid.Parse(rid); err != nil {
				rid = logger.GenerateRequestID()
			}
			entry := log.WithField(logger.RequestIDField, rid).WithFields(logrus.Fields{
				"method": r.Method,
				"path":   r.URL.Path,
			})
			ctx := logger.ContextWithLogger(r.Context(), entry)
			ctx = logger.ContextWithRequestID(ctx, rid)

			w.Header().Set(requestIDHeader, rid)
			start := time.Now()
			entry.Info("request started")
			rec := &responseRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r.WithContext(ctx))
			entry.WithFields(logrus.Fields{
				"status":      rec.status,
				"duration_ms": time.Since(start).Milliseconds(),
			}).Info("request finished")
		})
	}
}

func withRecovery() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.GetLoggerFromContext(r.Context()).
						WithField("panic", rec).
						WithField("stack", string(debug.Stack())).
						Error("recovered panic")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
