package build

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/common/model"
)

// debugLogBenchVec is a service-graph vector whose every series walks one of
// the per-series Debug paths of parseServiceGraph: the missing-UID non-URL
// fallback, a resolved and an unresolvable connection string, a host that is
// not Kubernetes DNS, a "://" label shadowed by a populated UID, and a
// wholly-empty side. Labels vary so the resolver's dedupe maps grow as they
// would over a real estate.
func debugLogBenchVec(n int) model.Vector {
	vec := make(model.Vector, 0, n)
	for i := range n {
		var s model.Sample
		switch i % 6 {
		case 0:
			s = famSample("checkout", fmt.Sprintf("legacy-%d", i%64), "prod-1", "abc", "")
		case 1:
			s = famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", "")
		case 2:
			s = famSample("checkout", fmt.Sprintf("amqp://app:s3cret@queue%d.data.svc:5672", i%64), "prod-1", "abc", "")
		case 3:
			s = famSample("checkout", fmt.Sprintf("redis://cache%d:6379", i%64), "prod-1", "abc", "")
		case 4:
			s = famSample("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", "n1")
		default:
			s = famSample("checkout", "", "prod-1", "abc", "")
		}
		s.Metric["series"] = model.LabelValue(fmt.Sprint(i)) // distinct series identity
		vec = append(vec, &s)
	}
	return vec
}

// withLogLevel installs a discarding handler at level for the benchmark's
// duration, so the measurement does not depend on the ambient default logger.
func withLogLevel(b *testing.B, level slog.Level) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level})))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// BenchmarkParseServiceGraph_DebugOff measures the per-series cost of the
// Debug evidence lines when Debug is NOT enabled — the production default.
// Every allocation it reports beyond resolution itself is argument
// construction for a record the handler would discard.
func BenchmarkParseServiceGraph_DebugOff(b *testing.B) {
	withLogLevel(b, slog.LevelInfo)
	vec := debugLogBenchVec(6000)
	topo := familyTopology()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = parseServiceGraph(vec, topo)
	}
}

// BenchmarkParseServiceGraph_DebugOn is the same parse with Debug enabled,
// the upper bound the gate must not change.
func BenchmarkParseServiceGraph_DebugOn(b *testing.B) {
	withLogLevel(b, slog.LevelDebug)
	vec := debugLogBenchVec(6000)
	topo := familyTopology()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = parseServiceGraph(vec, topo)
	}
}
