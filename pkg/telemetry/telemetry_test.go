package telemetry

import (
	"context"
	"io"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestInit_NoEndpoint_ReturnsNoopShutdown(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	lg := logrus.New()
	lg.SetOutput(io.Discard)
	shutdown, err := Init(context.Background(), "test-service", logrus.NewEntry(lg))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned err: %v", err)
	}
}

func TestNoopMetrics_AllInstrumentsUsable(t *testing.T) {
	m := NoopMetrics()
	if m == nil {
		t.Fatal("NoopMetrics returned nil")
	}
	ctx := context.Background()
	// All recordings should succeed without panic and without I/O.
	m.IngestTotal.Add(ctx, 1)
	m.IngestDuration.Record(ctx, 0.001)
	m.LLMCallTotal.Add(ctx, 1)
	m.LLMCallDuration.Record(ctx, 0.123)
	m.ReplySendTotal.Add(ctx, 1)
	m.ReplyDroppedTotal.Add(ctx, 1)
	m.EscalatedTotal.Add(ctx, 1)
	m.ClosedTotal.Add(ctx, 1)
	m.DigestSentTotal.Add(ctx, 1)
}
