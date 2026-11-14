package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// NewSlogHandler returns a slog.Handler that writes records to the supplied
// local handler and, when telemetry is enabled, mirrors them to the global
// OTLP LoggerProvider via otelslog. The local handler's output is enriched
// with trace_id / span_id keys whenever a record is emitted from a context
// carrying an active span.
//
// level is the configured --log-level, and it gates the OTLP bridge: the
// bridge's own Enabled asks the SDK LoggerProvider, whose batch processor
// accepts every level, so without this filter every Debug record would be
// built and exported whatever the operator configured — and a burst of them
// overflows the batch queue and drops the Info / Warn records that matter.
// Pass the SAME level the local handler uses (observability.ParseLevel), so a
// record is written once to each sink or to neither. Nil means Info.
func NewSlogHandler(local slog.Handler, level slog.Leveler) slog.Handler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &fanoutHandler{
		local:  &spanCorrelatedHandler{inner: local},
		bridge: &levelFilter{inner: otelslog.NewHandler(ServiceName), level: level},
	}
}

// levelFilter drops records below level before they reach inner. WithAttrs
// and WithGroup re-wrap, so a logger derived with With(...) keeps the filter.
type levelFilter struct {
	inner slog.Handler
	level slog.Leveler
}

func (h *levelFilter) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level.Level() && h.inner.Enabled(ctx, level)
}

func (h *levelFilter) Handle(ctx context.Context, record slog.Record) error {
	return h.inner.Handle(ctx, record)
}

func (h *levelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelFilter{inner: h.inner.WithAttrs(attrs), level: h.level}
}

func (h *levelFilter) WithGroup(name string) slog.Handler {
	return &levelFilter{inner: h.inner.WithGroup(name), level: h.level}
}

// fanoutHandler delivers each record to two underlying handlers. The bridge
// is a no-op when the global LoggerProvider is no-op (i.e. tracing disabled).
type fanoutHandler struct {
	local  slog.Handler
	bridge slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.local.Enabled(ctx, level) || h.bridge.Enabled(ctx, level)
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	if h.local.Enabled(ctx, record.Level) {
		if err := h.local.Handle(ctx, record.Clone()); err != nil {
			return err
		}
	}
	if h.bridge.Enabled(ctx, record.Level) {
		if err := h.bridge.Handle(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{
		local:  h.local.WithAttrs(attrs),
		bridge: h.bridge.WithAttrs(attrs),
	}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{
		local:  h.local.WithGroup(name),
		bridge: h.bridge.WithGroup(name),
	}
}

// spanCorrelatedHandler injects trace_id and span_id into every record whose
// context carries a recording span. The OTLP bridge already pulls span context
// directly from ctx, so this handler only enriches the local stderr stream.
type spanCorrelatedHandler struct {
	inner slog.Handler
}

func (h *spanCorrelatedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *spanCorrelatedHandler) Handle(ctx context.Context, record slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, record)
}

func (h *spanCorrelatedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &spanCorrelatedHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *spanCorrelatedHandler) WithGroup(name string) slog.Handler {
	return &spanCorrelatedHandler{inner: h.inner.WithGroup(name)}
}
