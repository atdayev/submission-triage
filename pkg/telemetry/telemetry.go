// Package telemetry sets up OpenTelemetry metrics, no-op when no endpoint is set.
package telemetry

import (
	"context"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Init sets the global meter provider; the returned func flushes at shutdown.
func Init(ctx context.Context, serviceName string, log *logrus.Entry) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		log.Info("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT not set; using no-op meter provider")
		otel.SetMeterProvider(noop.NewMeterProvider())
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(30*time.Second))),
	)
	otel.SetMeterProvider(mp)
	log.WithField("endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")).
		Info("telemetry: OTLP meter provider initialized")
	return mp.Shutdown, nil
}

// Metrics holds the instruments the service records into.
type Metrics struct {
	IngestTotal       metric.Int64Counter
	IngestDuration    metric.Float64Histogram
	LLMCallTotal      metric.Int64Counter
	LLMCallDuration   metric.Float64Histogram
	ReplySendTotal    metric.Int64Counter
	ReplyDroppedTotal metric.Int64Counter
	EscalatedTotal    metric.Int64Counter
	ClosedTotal       metric.Int64Counter
	DigestSentTotal   metric.Int64Counter
}

// NewMetrics builds the instrument set. Call after Init.
func NewMetrics() (*Metrics, error) {
	m := otel.Meter("github.com/atdayev/submission-triage")
	ingestTotal, err := m.Int64Counter("submission_triage_ingest_total",
		metric.WithDescription("Inbound emails ingested, labeled by final state and duplicate flag."))
	if err != nil {
		return nil, err
	}
	ingestDur, err := m.Float64Histogram("submission_triage_ingest_duration_seconds",
		metric.WithDescription("Wall-clock duration of IngestEmail."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	llmTotal, err := m.Int64Counter("submission_triage_llm_call_total",
		metric.WithDescription("LLM calls made, labeled by model, op, and outcome."))
	if err != nil {
		return nil, err
	}
	llmDur, err := m.Float64Histogram("submission_triage_llm_call_duration_seconds",
		metric.WithDescription("Wall-clock duration of LLM calls reported by the Usage payload."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	replyTotal, err := m.Int64Counter("submission_triage_reply_send_total",
		metric.WithDescription("Reply emails attempted, labeled by status."))
	if err != nil {
		return nil, err
	}
	replyDropped, err := m.Int64Counter("submission_triage_reply_dropped_total",
		metric.WithDescription("Replies dropped because the worker pool queue was saturated."))
	if err != nil {
		return nil, err
	}
	esc, err := m.Int64Counter("submission_triage_escalated_total",
		metric.WithDescription("Submissions transitioned to Escalated by the worker."))
	if err != nil {
		return nil, err
	}
	closed, err := m.Int64Counter("submission_triage_closed_total",
		metric.WithDescription("Submissions auto-closed after the Complete quiet period."))
	if err != nil {
		return nil, err
	}
	digest, err := m.Int64Counter("submission_triage_digest_sent_total",
		metric.WithDescription("Escalation digest emails dispatched."))
	if err != nil {
		return nil, err
	}
	return &Metrics{
		IngestTotal:       ingestTotal,
		IngestDuration:    ingestDur,
		LLMCallTotal:      llmTotal,
		LLMCallDuration:   llmDur,
		ReplySendTotal:    replyTotal,
		ReplyDroppedTotal: replyDropped,
		EscalatedTotal:    esc,
		ClosedTotal:       closed,
		DigestSentTotal:   digest,
	}, nil
}

// NoopMetrics returns no-op instruments for tests.
func NoopMetrics() *Metrics {
	otel.SetMeterProvider(noop.NewMeterProvider())
	m, _ := NewMetrics()
	return m
}
