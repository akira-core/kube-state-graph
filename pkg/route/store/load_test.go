package store

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// A scripted driver.Conn: each Query is answered by the first route whose
// table-and-predicate marker the SQL contains. It records every query and its
// arguments, so the tests can pin both the SQL shape and the bindings.
// ---------------------------------------------------------------------------

type scriptedConn struct {
	driver.Conn // only Query is implemented; anything else panics

	mu     sync.Mutex
	routes []scriptRoute
	seen   []seenQuery
}

type scriptRoute struct {
	marker string
	rows   [][]any
}

type seenQuery struct {
	sql  string
	args []any
}

func (c *scriptedConn) Query(_ context.Context, sql string, args ...any) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, seenQuery{sql: sql, args: args})
	for _, r := range c.routes {
		if strings.Contains(sql, r.marker) {
			return &scriptedRows{rows: r.rows, i: -1}, nil
		}
	}
	return &scriptedRows{i: -1}, nil
}

func (c *scriptedConn) query(marker string) (seenQuery, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, q := range c.seen {
		if strings.Contains(q.sql, marker) {
			return q, true
		}
	}
	return seenQuery{}, false
}

type scriptedRows struct {
	driver.Rows
	rows [][]any
	i    int
}

func (r *scriptedRows) Next() bool   { r.i++; return r.i < len(r.rows) }
func (r *scriptedRows) Err() error   { return nil }
func (r *scriptedRows) Close() error { return nil }
func (r *scriptedRows) Scan(dest ...any) error {
	for j, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(r.rows[r.i][j]))
	}
	return nil
}

const (
	markIngress = "FROM service_versions WHERE cluster = ? AND (has(external_ips"
	markBackend = "concat(namespace, '/', name)"
	markDeploy  = "FROM deploy_versions"
	markGateway = "FROM gw_versions"
	markVS      = "FROM vs_versions"
)

var (
	loadAt   = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	liveFrom = loadAt.Add(-time.Hour)
)

func svcRowVals(ns, name string, lbIPs, selector []string, spec string) []any {
	return []any{"prod-01", ns, name, liveFrom, sentinel, []string(nil), lbIPs, selector, spec, uint64(1)}
}

func vsRowVals(ns, name string, bound []string, spec string) []any {
	return []any{"prod-01", ns, name, liveFrom, sentinel, bound, spec, uint64(1)}
}

// ingressScript is one IP's state: an ingress LB Service + Deployment in
// istio-system, one Gateway there, a good VirtualService routing to shop/api,
// a VirtualService whose spec is not JSON, and the shop/api backend Service.
func ingressScript() []scriptRoute {
	goodVS := `{"hosts":["api.example.com"],"gateways":["public-gw"],"http":[{"route":[{"destination":{"host":"api.shop.svc.cluster.local"}}]}]}`
	return []scriptRoute{
		{marker: markIngress, rows: [][]any{
			svcRowVals("istio-system", "ingress-lb", []string{"198.51.100.7"}, []string{"istio=ingress"}, ""),
		}},
		{marker: markDeploy, rows: [][]any{
			{"prod-01", "istio-system", "ingress", liveFrom, sentinel, []string{"istio=ingress"}, uint64(1)},
		}},
		{marker: markGateway, rows: [][]any{
			{"prod-01", "istio-system", "public-gw", liveFrom, sentinel, []string{"istio=ingress"}, []string{"*.example.com"}, `{}`, uint64(1)},
		}},
		{marker: markVS, rows: [][]any{
			vsRowVals("istio-system", "vs-good", []string{"public-gw"}, goodVS),
			vsRowVals("istio-system", "vs-broken", []string{"public-gw"}, `{not json`),
		}},
		{marker: markBackend, rows: [][]any{
			svcRowVals("shop", "api", nil, nil, `{"ports":[{"name":"http","port":8080}]}`),
			svcRowVals("shop", "broken", nil, nil, `{"ports":"nope"}`),
		}},
	}
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// #16: one VirtualService (or Service) row whose spec does not parse is
// skipped with a warning naming it; the IP's load succeeds and every other
// row comes back. Before, the one bad row failed the whole IP.
func TestLoadTrafficAt_SkipsUnparseableRows(t *testing.T) {
	logs := captureWarnings(t)
	conn := &scriptedConn{routes: ingressScript()}
	s := &CH{conn: conn}

	w, err := s.LoadTrafficAt(t.Context(), "prod-01", "198.51.100.7", loadAt)
	require.NoError(t, err)

	require.Len(t, w.VSes, 1, "the unparseable VirtualService is dropped from the snapshot too")
	assert.Equal(t, "vs-good", w.VSes[0].Name)
	var names []string
	for _, r := range w.Services {
		names = append(names, r.Namespace+"/"+r.Name)
	}
	assert.Equal(t, []string{"istio-system/ingress-lb", "shop/api"}, names)
	assert.Contains(t, logs.String(), "name=vs-broken")
	assert.Contains(t, logs.String(), "name=broken")
}

// #16: bare bindings are scoped to the candidate gateways' namespaces, while
// the qualified form loads from anywhere; the index-friendly superset stays.
func TestLoadTrafficAt_VirtualServiceBindingsAreNamespaceScoped(t *testing.T) {
	conn := &scriptedConn{routes: ingressScript()}
	s := &CH{conn: conn}

	_, err := s.LoadTrafficAt(t.Context(), "prod-01", "198.51.100.7", loadAt)
	require.NoError(t, err)

	q, ok := conn.query(markVS)
	require.True(t, ok)
	assert.Contains(t, q.sql, "hasAny(bound_gateways, ?) AND (hasAny(bound_gateways, ?) OR has(?, namespace))")
	assert.Equal(t, []any{
		"prod-01",
		[]string{"istio-system/public-gw", "public-gw"}, // superset: both forms
		[]string{"istio-system/public-gw"},              // qualified: any namespace
		[]string{"istio-system"},                        // bare: candidate namespaces only
	}, q.args)
}

func TestBindingsOf(t *testing.T) {
	b := bindingsOf([]GatewayRow{
		{Namespace: "istio-system", Name: "public-gw"},
		{Namespace: "istio-system", Name: "admin-gw"},
		{Namespace: "edge", Name: "public-gw"},
	})
	assert.Equal(t, []string{"istio-system/public-gw", "public-gw", "istio-system/admin-gw", "admin-gw", "edge/public-gw"}, b.refs)
	assert.Equal(t, []string{"istio-system/public-gw", "istio-system/admin-gw", "edge/public-gw"}, b.qualified)
	assert.Equal(t, []string{"istio-system", "edge"}, b.namespaces)
}

// P-6: in the default mode every version read dedups per slot on the server
// and only then filters liveness, in an OUTER query — valid_to never appears
// in the scanning query. Pruned mode keeps its single-query shape.
func TestVersionRead_Shapes(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	lit := dt64Lit(at)

	def := (&CH{}).versionRead("cluster, namespace, name, valid_from, valid_to, ingest_seq", "gw_versions", "cluster = ?", at)
	assert.Equal(t,
		"SELECT cluster, namespace, name, valid_from, valid_to, ingest_seq FROM ("+
			"SELECT cluster, namespace, name, valid_from, valid_to, ingest_seq FROM gw_versions "+
			"WHERE cluster = ? AND valid_from <= "+lit+" "+
			"ORDER BY ingest_seq DESC LIMIT 1 BY cluster, namespace, name, valid_from"+
			") WHERE "+lit+" < valid_to",
		def)
	inner, outer, ok := strings.Cut(def, ") WHERE ")
	require.True(t, ok)
	_, scan, _ := strings.Cut(inner, " WHERE ")
	scan, _, _ = strings.Cut(scan, " ORDER BY ")
	assert.NotContains(t, scan, "valid_to", "the deduplicating scan must not filter valid_to")
	assert.Equal(t, lit+" < valid_to", outer)

	pruned := (&CH{uniqueRows: true}).versionRead("cluster, valid_to", "gw_versions", "cluster = ?", at)
	assert.Equal(t, "SELECT cluster, valid_to FROM gw_versions WHERE cluster = ? AND valid_from <= "+lit+" AND "+lit+" < valid_to", pruned)
	assert.NotContains(t, pruned, "LIMIT 1 BY")
}

// Every read of a load — and the cross-cluster probe — goes through
// versionRead.
func TestLoadTrafficAt_EveryReadDedupsOnTheServer(t *testing.T) {
	conn := &scriptedConn{routes: ingressScript()}
	s := &CH{conn: conn}
	_, err := s.LoadTrafficAt(t.Context(), "prod-01", "198.51.100.7", loadAt)
	require.NoError(t, err)
	_, err = s.ClustersWithIngressIP(t.Context(), "198.51.100.7", loadAt)
	require.NoError(t, err)

	require.Len(t, conn.seen, 6)
	for _, q := range conn.seen {
		assert.Contains(t, q.sql, "LIMIT 1 BY cluster, namespace, name, valid_from", q.sql)
	}
}
