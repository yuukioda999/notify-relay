// Package telemetry sends traces and metrics with OpenTelemetry over OTLP HTTP (notify-relay
// ADR-0004, team-ops ADR-0026). It implements usecase.Telemetry and adds the HTTP server span.
//
// Nothing personal is recorded: spans carry IDs, counts and fixed codes only, and metric labels
// take a small fixed set of values (team-ops AC-010-01, AC-010-06).
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

const scope = "github.com/yuukioda999/notify-relay"

// sendBuckets are the histogram boundaries for one SMTP send, in seconds.
var sendBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Telemetry records the spans and metrics of notify-relay.
type Telemetry struct {
	tracer             trace.Tracer
	meter              metric.Meter
	prop               propagation.TextMapPropagator
	accepted           metric.Int64Counter
	acceptedRecipients metric.Int64Counter
	sends              metric.Int64Counter
	sendDuration       metric.Float64Histogram
	webhookFailures    metric.Int64Counter
}

var _ usecase.Telemetry = (*Telemetry)(nil)

// Enabled reports whether traces and metrics should be exported: an OTLP endpoint is set and
// OTEL_SDK_DISABLED is not true (load tests turn it off, team-ops ADR-0026).
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Setup creates the exporters from the standard OTEL_* environment variables. When Enabled is
// false nothing is exported, but a traceparent received with a request is still passed on.
// The returned function flushes and stops the exporters.
func Setup(ctx context.Context) (*Telemetry, func(context.Context) error, error) {
	if !Enabled() {
		t, err := New(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())
		return t, func(context.Context) error { return nil }, err
	}

	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = "notify-relay"
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", name)))
	if err != nil {
		return nil, nil, err
	}
	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))
	t, err := New(tp, mp)
	if err != nil {
		return nil, nil, errors.Join(err, tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return t, func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }, nil
}

// New builds a Telemetry on the given providers. Tests pass in-memory providers.
func New(tp trace.TracerProvider, mp metric.MeterProvider) (*Telemetry, error) {
	m := mp.Meter(scope)
	t := &Telemetry{tracer: tp.Tracer(scope), meter: m, prop: propagation.TraceContext{}}
	var errs [5]error
	t.accepted, errs[0] = m.Int64Counter("notify_relay.deliveries.accepted",
		metric.WithUnit("{delivery}"), metric.WithDescription("Delivery requests accepted (repeats not counted)"))
	t.acceptedRecipients, errs[1] = m.Int64Counter("notify_relay.recipients.accepted",
		metric.WithUnit("{recipient}"), metric.WithDescription("Recipients in accepted delivery requests"))
	t.sends, errs[2] = m.Int64Counter("notify_relay.send.attempts",
		metric.WithUnit("{attempt}"), metric.WithDescription("Send attempts by result and error code"))
	t.sendDuration, errs[3] = m.Float64Histogram("notify_relay.send.duration",
		metric.WithUnit("s"), metric.WithDescription("Time the mail server took for one send"),
		metric.WithExplicitBucketBoundaries(sendBuckets...))
	t.webhookFailures, errs[4] = m.Int64Counter("notify_relay.webhook.failures",
		metric.WithUnit("{event}"), metric.WithDescription("Webhooks the business app did not accept, by reason"))
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return t, nil
}

// RegisterUnsent reports the number of recipients waiting or being sent, read when metrics
// are collected (AC-R001-24).
func (t *Telemetry) RegisterUnsent(count func(context.Context) (int64, error)) error {
	_, err := t.meter.Int64ObservableGauge("notify_relay.recipients.unsent",
		metric.WithUnit("{recipient}"), metric.WithDescription("Recipients waiting to be sent or being sent"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			n, err := count(ctx)
			if err != nil {
				return err
			}
			o.Observe(n)
			return nil
		}))
	return err
}

// Accepted counts a newly accepted delivery.
func (t *Telemetry) Accepted(ctx context.Context, recipients int) {
	t.accepted.Add(ctx, 1)
	t.acceptedRecipients.Add(ctx, int64(recipients))
}

// StartSend starts a span under the traceparent stored with the delivery (AC-R001-25).
func (t *Telemetry) StartSend(ctx context.Context, s usecase.SendSpan) (context.Context, func(usecase.SendResult, domain.ErrorCode, bool, time.Duration)) {
	ctx, span := t.tracer.Start(t.withParent(ctx, s.Traceparent), "send mail",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("notify_relay.delivery_id", string(s.DeliveryID)),
			attribute.Int64("notify_relay.recipient_id", s.RecipientID),
			attribute.Int("notify_relay.attempt", s.Attempt),
		))
	return ctx, func(result usecase.SendResult, code domain.ErrorCode, mailed bool, took time.Duration) {
		labels := metric.WithAttributes(attribute.String("result", string(result)), attribute.String("error_code", string(code)))
		span.SetAttributes(attribute.String("notify_relay.result", string(result)), attribute.String("notify_relay.error_code", string(code)))
		if result == usecase.SendFailed || result == usecase.SendRetry {
			span.SetStatus(codes.Error, string(code))
		}
		t.sends.Add(ctx, 1, labels)
		if mailed {
			t.sendDuration.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String("result", string(result))))
		}
		span.End()
	}
}

// StartWebhook starts a span under the stored traceparent and returns the span's own traceparent
// for the webhook request, so the business app continues the trace (AC-R001-25).
func (t *Telemetry) StartWebhook(ctx context.Context, eventID, traceparent string) (context.Context, string, func(usecase.WebhookFailure)) {
	ctx, span := t.tracer.Start(t.withParent(ctx, traceparent), "send webhook",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("notify_relay.webhook_event_id", eventID)))
	out := t.traceparentOf(ctx)
	if out == "" {
		out = traceparent
	}
	return ctx, out, func(failure usecase.WebhookFailure) {
		if failure != "" {
			span.SetStatus(codes.Error, string(failure))
			t.webhookFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", string(failure))))
		}
		span.End()
	}
}

// untracedPaths are polled by health checks every few seconds; spans for them are only noise.
var untracedPaths = map[string]bool{"/healthz": true, "/readyz": true}

// Middleware starts a server span for each request, continuing the caller's traceparent. Only the
// method, the route pattern and the status code are recorded: never the URL, its query or a header.
func (t *Telemetry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if untracedPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		ctx := t.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := t.tracer.Start(ctx, r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r = r.WithContext(ctx)
		next.ServeHTTP(rec, r)

		// ServeMux sets the matched pattern, such as "POST /v1/deliveries", on the request.
		if _, route, ok := strings.Cut(r.Pattern, " "); ok {
			span.SetName(r.Method + " " + route)
			span.SetAttributes(attribute.String("http.route", route))
		}
		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "")
		}
	})
}

// Traceparent returns the trace context of the request's server span, to be stored with a new
// delivery. Without a span it is the caller's traceparent when it is well formed, or "".
func (t *Telemetry) Traceparent(r *http.Request) string {
	return t.traceparentOf(r.Context())
}

func (t *Telemetry) traceparentOf(ctx context.Context) string {
	c := propagation.MapCarrier{}
	t.prop.Inject(ctx, c)
	return c["traceparent"]
}

func (t *Telemetry) withParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return t.prop.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// LogHandler adds trace_id and span_id to log records written with a context that has a span,
// so a log line can be found from its trace and back (team-ops AC-010-13).
type LogHandler struct{ slog.Handler }

// Handle adds the IDs and passes the record on.
func (h LogHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec = rec.Clone()
		rec.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs keeps the wrapper.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup keeps the wrapper.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{h.Handler.WithGroup(name)}
}
