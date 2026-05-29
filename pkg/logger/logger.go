package logger

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	"github.com/sirupsen/logrus"
)

type ctxKey int

const (
	loggerCtxKey ctxKey = iota
	requestIDCtxKey
)

const RequestIDField = "request_id"

func SetupLogger(level, format, serviceName, logDir string, maxAgeDays, rotationHours int) (*logrus.Entry, func() error, error) {
	lg := logrus.New()

	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		return nil, nil, fmt.Errorf("parse log level %q: %w", level, err)
	}
	lg.SetLevel(lvl)

	switch strings.ToLower(format) {
	case "json":
		lg.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	default:
		lg.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: time.RFC3339,
		})
	}

	writers := []io.Writer{os.Stdout}
	closer := func() error { return nil }

	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create log dir %s: %w", logDir, err)
		}
		pattern := filepath.Join(logDir, serviceName+".%Y%m%d.log")
		opts := []rotatelogs.Option{
			rotatelogs.WithMaxAge(time.Duration(maxAgeDays) * 24 * time.Hour),
			rotatelogs.WithRotationTime(time.Duration(rotationHours) * time.Hour),
		}
		// The "current log" symlink needs privileges Windows doesn't grant
		// a normal process (Admin / Developer Mode). Skip it there; the
		// dated files are still written.
		if runtime.GOOS != "windows" {
			opts = append(opts, rotatelogs.WithLinkName(filepath.Join(logDir, serviceName+".log")))
		}
		rl, err := rotatelogs.New(pattern, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("init rotating log: %w", err)
		}
		writers = append(writers, rl)
		closer = rl.Close
	}

	lg.SetOutput(io.MultiWriter(writers...))

	entry := lg.WithField("service", serviceName)
	return entry, closer, nil
}

func ContextWithLogger(ctx context.Context, entry *logrus.Entry) context.Context {
	return context.WithValue(ctx, loggerCtxKey, entry)
}

func GetLoggerFromContext(ctx context.Context) *logrus.Entry {
	if ctx == nil {
		return logrus.NewEntry(logrus.StandardLogger())
	}
	if v, ok := ctx.Value(loggerCtxKey).(*logrus.Entry); ok && v != nil {
		return v
	}
	return logrus.NewEntry(logrus.StandardLogger())
}

func ContextWithRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey, rid)
}

func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDCtxKey).(string); ok {
		return v
	}
	return ""
}

func GenerateRequestID() string {
	return uuid.NewString()
}
