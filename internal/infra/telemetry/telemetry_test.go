package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

const (
	parentTrace = "0af7651916cd43dd8448eb211c80319c"
	parentSpan  = "b7ad6b7169203331"
	parent      = "00-" + parentTrace + "-" + parentSpan + "-01"
)

type harness struct {
	tel    *Telemetry
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newHarness(t *testing.T) harness {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tel, err := New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)), sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	return harness{tel: tel, spans: spans, reader: reader}
}

func (h harness) metrics(t *testing.T) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func sumOf(t *testing.T, data metricdata.Aggregation, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	s, ok := data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("not an int64 sum: %T", data)
	}
	want := attribute.NewSet(attrs...)
	for _, dp := range s.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

func TestSendSpan(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		wantTrace   string // "" means a new trace
	}{
		{name: "AC-R001-25 a send continues the trace stored at acceptance", traceparent: parent, wantTrace: parentTrace},
		{name: "a delivery accepted without a traceparent starts its own trace", traceparent: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, end := h.tel.StartSend(context.Background(), usecase.SendSpan{DeliveryID: "dlv_1", RecipientID: 7, Attempt: 2, Traceparent: tt.traceparent})
			end(usecase.SendDelivered, "", true, 120*time.Millisecond)

			ended := h.spans.Ended()
			if len(ended) != 1 {
				t.Fatalf("%d spans ended, want 1", len(ended))
			}
			sc := ended[0].SpanContext()
			if tt.wantTrace != "" {
				if sc.TraceID().String() != tt.wantTrace || ended[0].Parent().SpanID().String() != parentSpan {
					t.Fatalf("span trace=%s parent=%s, want %s/%s", sc.TraceID(), ended[0].Parent().SpanID(), tt.wantTrace, parentSpan)
				}
			} else if ended[0].Parent().IsValid() {
				t.Fatal("span has a parent although none was stored")
			}
		})
	}
}

func TestWebhookSpan(t *testing.T) {
	t.Run("AC-R001-25 the webhook carries a traceparent of the same trace", func(t *testing.T) {
		h := newHarness(t)
		_, out, end := h.tel.StartWebhook(context.Background(), "evt_1", parent)
		end("")
		ended := h.spans.Ended()
		if len(ended) != 1 {
			t.Fatalf("%d spans ended, want 1", len(ended))
		}
		want := "00-" + parentTrace + "-" + ended[0].SpanContext().SpanID().String() + "-01"
		if out != want {
			t.Fatalf("traceparent = %q, want %q", out, want)
		}
	})

	t.Run("AC-R001-25 with tracing off the stored traceparent is passed on unchanged", func(t *testing.T) {
		tel, err := New(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())
		if err != nil {
			t.Fatal(err)
		}
		_, out, end := tel.StartWebhook(context.Background(), "evt_1", parent)
		end("")
		if out != parent {
			t.Fatalf("traceparent = %q, want %q", out, parent)
		}
	})
}

func TestMetrics(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.tel.Accepted(ctx, 3)
	h.tel.Accepted(ctx, 2)
	for _, r := range []struct {
		result usecase.SendResult
		code   domain.ErrorCode
		mailed bool
	}{
		{usecase.SendDelivered, "", true},
		{usecase.SendDelivered, "", true},
		{usecase.SendRetry, domain.ErrRateLimited, true},
		{usecase.SendSkipped, domain.ErrExpired, false},
	} {
		_, end := h.tel.StartSend(ctx, usecase.SendSpan{DeliveryID: "dlv_1", RecipientID: 1, Attempt: 1})
		end(r.result, r.code, r.mailed, 200*time.Millisecond)
	}
	for _, f := range []usecase.WebhookFailure{"", usecase.WebhookRetrying, usecase.WebhookRetrying, usecase.WebhookGaveUp} {
		_, _, end := h.tel.StartWebhook(ctx, "evt_1", "")
		end(f)
	}
	if err := h.tel.RegisterUnsent(func(context.Context) (int64, error) { return 42, nil }); err != nil {
		t.Fatal(err)
	}
	got := h.metrics(t)

	tests := []struct {
		name   string
		metric string
		attrs  []attribute.KeyValue
		want   int64
	}{
		{"AC-R001-24 accepted deliveries", "notify_relay.deliveries.accepted", nil, 2},
		{"AC-R001-24 accepted recipients", "notify_relay.recipients.accepted", nil, 5},
		{"AC-R001-24 sends by result: delivered", "notify_relay.send.attempts", []attribute.KeyValue{attribute.String("result", "delivered"), attribute.String("error_code", "")}, 2},
		{"AC-R001-24 sends by result: retry with its code", "notify_relay.send.attempts", []attribute.KeyValue{attribute.String("result", "retry"), attribute.String("error_code", "rate_limited")}, 1},
		{"AC-R001-24 sends by result: skipped", "notify_relay.send.attempts", []attribute.KeyValue{attribute.String("result", "skipped"), attribute.String("error_code", "expired")}, 1},
		{"AC-R001-24 webhook failures: retry", "notify_relay.webhook.failures", []attribute.KeyValue{attribute.String("reason", "retry")}, 2},
		{"AC-R001-24 webhook failures: gave up", "notify_relay.webhook.failures", []attribute.KeyValue{attribute.String("reason", "gave_up")}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v := sumOf(t, got[tt.metric], tt.attrs...); v != tt.want {
				t.Fatalf("%s%v = %d, want %d", tt.metric, tt.attrs, v, tt.want)
			}
		})
	}

	t.Run("AC-R001-24 send duration is recorded only for mail that went to the server", func(t *testing.T) {
		hist, ok := got["notify_relay.send.duration"].(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("send duration is %T", got["notify_relay.send.duration"])
		}
		var n uint64
		for _, dp := range hist.DataPoints {
			n += dp.Count
		}
		if n != 3 {
			t.Fatalf("duration count = %d, want 3", n)
		}
	})

	t.Run("AC-R001-24 unsent recipients are read when metrics are collected", func(t *testing.T) {
		g, ok := got["notify_relay.recipients.unsent"].(metricdata.Gauge[int64])
		if !ok || len(g.DataPoints) != 1 || g.DataPoints[0].Value != 42 {
			t.Fatalf("unsent = %+v", got["notify_relay.recipients.unsent"])
		}
	})

	t.Run("AC-010-06 metric labels are only result, error_code and reason", func(t *testing.T) {
		allowed := map[attribute.Key]bool{"result": true, "error_code": true, "reason": true}
		var rm metricdata.ResourceMetrics
		if err := h.reader.Collect(ctx, &rm); err != nil {
			t.Fatal(err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, set := range attributeSets(m.Data) {
					for _, kv := range set.ToSlice() {
						if !allowed[kv.Key] {
							t.Errorf("%s has label %s", m.Name, kv.Key)
						}
					}
				}
			}
		}
	})
}

func attributeSets(data metricdata.Aggregation) []attribute.Set {
	var out []attribute.Set
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	}
	return out
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		header     string
		wantSpans  int
		wantParent string
	}{
		{name: "AC-010-02 a request continues the caller's trace", path: "/v1/deliveries", header: parent, wantSpans: 1, wantParent: parentSpan},
		{name: "a request without a traceparent starts a trace", path: "/v1/deliveries", wantSpans: 1},
		{name: "health checks make no spans", path: "/healthz", wantSpans: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			var stored string
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/deliveries", func(w http.ResponseWriter, r *http.Request) {
				stored = h.tel.Traceparent(r)
				w.WriteHeader(http.StatusAccepted)
			})
			mux.HandleFunc("POST /healthz", func(http.ResponseWriter, *http.Request) {})
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path+"?email=parent%40example.com", nil)
			if tt.header != "" {
				r.Header.Set("traceparent", tt.header)
			}
			h.tel.Middleware(mux).ServeHTTP(httptest.NewRecorder(), r)

			ended := h.spans.Ended()
			if len(ended) != tt.wantSpans {
				t.Fatalf("%d spans, want %d", len(ended), tt.wantSpans)
			}
			if tt.wantSpans == 0 {
				return
			}
			s := ended[0]
			if s.Name() != "POST /v1/deliveries" {
				t.Fatalf("span name = %q", s.Name())
			}
			if tt.wantParent != "" && (s.Parent().SpanID().String() != tt.wantParent || s.SpanContext().TraceID().String() != parentTrace) {
				t.Fatalf("parent = %s, trace = %s", s.Parent().SpanID(), s.SpanContext().TraceID())
			}
			// AC-R001-25: the delivery stores the server span, so sending and webhooks hang under it.
			want := "00-" + s.SpanContext().TraceID().String() + "-" + s.SpanContext().SpanID().String() + "-01"
			if stored != want {
				t.Fatalf("stored traceparent = %q, want %q", stored, want)
			}
			// AC-010-01: no URL, query or address is recorded.
			for _, kv := range s.Attributes() {
				if v := kv.Value.Emit(); strings.Contains(v, "@") || strings.Contains(v, "email") || strings.Contains(v, "?") {
					t.Fatalf("attribute %s = %q leaks the query", kv.Key, v)
				}
			}
			if code := attrInt(s.Attributes(), "http.response.status_code"); code != http.StatusAccepted {
				t.Fatalf("status code = %d", code)
			}
		})
	}
}

func attrInt(attrs []attribute.KeyValue, key attribute.Key) int64 {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value.AsInt64()
		}
	}
	return 0
}

func TestSendSpanHoldsNoPersonalData(t *testing.T) {
	t.Run("AC-010-01 the send span has IDs and codes only", func(t *testing.T) {
		h := newHarness(t)
		_, end := h.tel.StartSend(context.Background(), usecase.SendSpan{DeliveryID: "dlv_1", RecipientID: 7, Attempt: 1})
		end(usecase.SendFailed, domain.ErrInvalidAddress, true, time.Second)
		allowed := map[attribute.Key]bool{
			"notify_relay.delivery_id": true, "notify_relay.recipient_id": true, "notify_relay.attempt": true,
			"notify_relay.result": true, "notify_relay.error_code": true,
		}
		for _, kv := range h.spans.Ended()[0].Attributes() {
			if !allowed[kv.Key] {
				t.Errorf("unexpected attribute %s", kv.Key)
			}
		}
	})
}

func TestLogHandler(t *testing.T) {
	tests := []struct {
		name      string
		ctx       func(*Telemetry) context.Context
		wantTrace string
	}{
		{name: "AC-010-13 a log written inside a span carries its trace ID", ctx: func(tel *Telemetry) context.Context {
			ctx, _ := tel.StartSend(context.Background(), usecase.SendSpan{Traceparent: parent})
			return ctx
		}, wantTrace: parentTrace},
		{name: "a log written outside a span has no trace ID", ctx: func(*Telemetry) context.Context { return context.Background() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			var buf bytes.Buffer
			log := slog.New(LogHandler{slog.NewJSONHandler(&buf, nil)}).With("delivery_id", "dlv_1")
			log.InfoContext(tt.ctx(h.tel), "send attempt finished")
			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatal(err)
			}
			got, _ := line["trace_id"].(string)
			if got != tt.wantTrace {
				t.Fatalf("trace_id = %q, want %q", got, tt.wantTrace)
			}
			if line["delivery_id"] != "dlv_1" {
				t.Fatal("attributes added with With were lost")
			}
		})
	}
}
