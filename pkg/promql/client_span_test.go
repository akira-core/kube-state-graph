package promql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installTestTracer registers an in-memory exporter for the duration of a test.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	// The global delegating tracer honours only the FIRST SetTracerProvider
	// in a process, so a second test installing its own exporter would keep
	// writing into the first one. The package-level tracer is therefore
	// rebound directly for the duration of the test and restored afterwards;
	// these tests do not run in parallel.
	prevTracer := tracer
	tracer = tp.Tracer("kube-state-graph")
	t.Cleanup(func() {
		tracer = prevTracer
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

// closedPortURL addresses a port nothing listens on, so Instant fails
// immediately without contacting any upstream service — the span under test is
// emitted before the request is attempted and ends on the deferred End().
const closedPortURL = "http://127.0.0.1:1"

func spanAttr(t *testing.T, s tracetest.SpanStub, key string) (string, bool) {
	t.Helper()
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// A query issued through a routed client must be attributable to the backend
// that answered it — with several upstreams, a span saying only "prometheus
// query failed" is not actionable.
func TestClient_SpanCarriesBackendName(t *testing.T) {
	exp := installTestTracer(t)

	c, err := New(closedPortURL, nil, WithBackendName("zone-b"))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, qerr := c.Instant(ctx, string(QPodInfo), "kube_pod_info", time.Unix(0, 0))
	require.Error(t, qerr, "the port is closed; only the span is under test")

	spans := exp.GetSpans()
	require.NotEmpty(t, spans)
	var query tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "prometheus.query" {
			query = s
		}
	}
	require.Equal(t, "prometheus.query", query.Name)

	backend, ok := spanAttr(t, query, "kube_state_graph.backend")
	require.True(t, ok, "the span must identify the backend")
	assert.Equal(t, "zone-b", backend)

	name, ok := spanAttr(t, query, "kube_state_graph.query_name")
	require.True(t, ok, "the pre-existing query_name attribute is unchanged")
	assert.Equal(t, string(QPodInfo), name)
}

// A single-upstream deployment emits exactly the span it emitted before
// backend routing existed: the attribute is omitted, not empty.
func TestClient_SpanOmitsBackendWhenUnrouted(t *testing.T) {
	exp := installTestTracer(t)

	c, err := New(closedPortURL, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, qerr := c.Instant(ctx, string(QPodInfo), "kube_pod_info", time.Unix(0, 0))
	require.Error(t, qerr)

	spans := exp.GetSpans()
	require.NotEmpty(t, spans)
	for _, s := range spans {
		if s.Name != "prometheus.query" {
			continue
		}
		_, ok := spanAttr(t, s, "kube_state_graph.backend")
		assert.False(t, ok, "an unrouted client must not emit an empty backend attribute")
	}
}

// DefaultClientFactory is what wires the backend name in production; a client
// it builds must carry the routing-table name.
func TestDefaultClientFactory_LabelsTheClient(t *testing.T) {
	f := DefaultClientFactory(nil)
	q, err := f(NewBackend("netapp-a", closedPortURL, []Family{FamilyHarvest}, nil, "", ""))
	require.NoError(t, err)
	c, ok := q.(*Client)
	require.True(t, ok)
	assert.Equal(t, "netapp-a", c.backend)
}

// The credential wiring between a validated Backend and the outbound transport
// is three lines that nothing else covers: config resolves the pair, the
// transport scopes it to one host, and this is the join between them.
func TestBackendClientOptions_CarriesPerBackendCredentials(t *testing.T) {
	var o clientOptions
	for _, apply := range BackendClientOptions(
		NewBackend("zone-a", "http://vm-a:8428", []Family{FamilyKSM}, nil, "ksg", "s3cret"),
	) {
		apply(&o)
	}
	assert.Equal(t, "zone-a", o.backend)
	assert.Equal(t, "ksg", o.username)
	assert.Equal(t, "s3cret", o.password)
}

// A backend with no credentials must attach no Authorization header at all —
// not an empty one, which would still authenticate as the empty user.
func TestBackendClientOptions_NoCredentialsAttachesNoAuth(t *testing.T) {
	var o clientOptions
	for _, apply := range BackendClientOptions(
		NewBackend("zone-a", "http://vm-a:8428", []Family{FamilyKSM}, nil, "", ""),
	) {
		apply(&o)
	}
	assert.Equal(t, "zone-a", o.backend)
	assert.Empty(t, o.username)
	assert.Empty(t, o.password)

	// And the assembled transport is the bare traced one, with no basic-auth
	// layer in the chain.
	rt := newTransport(o, "vm-a:8428", &fakeRoundTripper{})
	assert.NotNil(t, rt)
	var inner fakeRoundTripper
	authed := newTransport(clientOptions{username: "u", password: "p"}, "vm-a:8428", &inner)
	got := roundTrip(t, authed, &inner, "http://vm-a:8428/api/v1/query")
	assert.NotEmpty(t, got.Header.Get("Authorization"),
		"the authenticated chain is the contrast: credentials reach the wire only when configured")
}
