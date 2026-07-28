package store

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"google.golang.org/protobuf/encoding/protojson"
	networking "istio.io/api/networking/v1alpha3"
)

// CH is the ClickHouse-backed read-only Store. All version tables are written
// by the metadata-exporter; every query here is a scoped read with a
// `cluster = ?` predicate.
//
// # Duplicate-row handling (why queries do NOT use FINAL)
//
// The exporter closes a version by REWRITING the previous open row — same
// ORDER BY key, higher ingest_seq, valid_to pulled in — so until background
// merges collapse the pair, both the stale open row (valid_to = far-future
// sentinel) and its closing rewrite are visible. The same duplicate shape
// appears on writer restart/replay. Something must therefore apply the
// ReplacingMergeTree rule (max ingest_seq per key) at read time.
//
// FINAL does that server-side but pays the part-merge machinery on every
// query (~10x lookup latency on the POC bench corpus) and silently depends on
// the server not moving the WHERE into PREWHERE ahead of the merge. Every
// reader here follows the POC's no-FINAL pattern instead:
//
//  1. SQL WHERE carries only IMMUTABLE predicates (cluster, join keys,
//     valid_from). valid_to must NEVER be filtered in SQL: the closing
//     rewrite (small valid_to) would be dropped pre-dedup while its stale
//     twin (sentinel valid_to) passes — the stale row would then win dedup
//     unopposed and a dead version would appear live. Same trap as putting
//     valid_to in WHERE instead of HAVING with an argMax rewrite.
//  2. The client dedups rows per version slot (cluster, namespace, name,
//     valid_from), keeping the highest ingest_seq (dedupLatest) — the exact
//     collapse ReplacingMergeTree performs at merge time.
//  3. The liveness predicate on valid_to (`at < valid_to`) is applied AFTER
//     dedup, in Go (dedupLiveAtCounted).
//
// Trade-off: step 1 fetches every version of the scoped keys with
// valid_from <= at, including versions that ended before that instant —
// bounded by per-key version counts (and table TTL), still scoped by the same
// join predicates. In exchange, scans run at plain-MergeTree speed with skip
// indexes fully effective.
//
// # Pruned mode (WithUniqueRows — update-close writers only)
//
// When the writer guarantees one physical row per version (exporter
// closeMode=update, after historical convergence), open the store with
// WithUniqueRows: every query then restores the valid_to predicate in SQL, so
// versions that ended at or before the resolution instant are pruned
// server-side instead of fetched. Steps 2 and 3 stay in place as a zero-cost safety net;
// CollapsedRows MUST read 0 — a positive count means duplicate version slots
// reached the reader and the writer's uniqueness guarantee is broken (alert
// on it). Do NOT enable against a rewrite-close writer: pruning would drop
// the closing rewrite before dedup and the stale sentinel twin would win
// unopposed.
//
// Time operands in every query are rendered as toDateTime64 literals
// (dt64Lit), never `?` binds — see dt64Lit for the driver trap.
type CH struct {
	conn driver.Conn

	// uniqueRows asserts the writer guarantees one physical row per version
	// slot, enabling server-side valid_to pruning. Never enable against the
	// exporter's default rewrite-close mode — see the package doc above.
	uniqueRows bool
	collapsed  atomic.Uint64
	warnOnce   sync.Once
}

var _ Store = (*CH)(nil)

// openConfig collects Option values before dial so auth can mutate chOpts
// prior to clickhouse.Open (credentials must be set at dial time).
type openConfig struct {
	uniqueRows bool
	username   string
	password   string
}

// Option tweaks store Open behaviour (auth, pruned-read mode).
type Option func(*openConfig)

// WithUniqueRows enables the pruned read mode for update-close writers ONLY.
// See the CH doc comment for the hazard against rewrite-close writers.
func WithUniqueRows() Option {
	return func(c *openConfig) { c.uniqueRows = true }
}

// WithAuth overrides ClickHouse native auth credentials after ParseDSN.
// Prefer this over embedding userinfo in the DSN so secrets stay in
// env-only config (KSG_ROUTE_STORE_USERNAME / KSG_ROUTE_STORE_PASSWORD).
// When username is non-empty, both Username and Password on chOpts.Auth
// are replaced (empty password is valid for some ClickHouse setups).
func WithAuth(username, password string) Option {
	return func(c *openConfig) {
		c.username = username
		c.password = password
	}
}

// applyAuth overlays openConfig credentials onto parsed ClickHouse options.
// Exported-for-test via the pure helper so unit tests need no live CH.
func applyAuth(chOpts *clickhouse.Options, cfg openConfig) {
	if cfg.username == "" {
		return
	}
	chOpts.Auth.Username = cfg.username
	chOpts.Auth.Password = cfg.password
}

// Open dials ClickHouse from a DSN (e.g. "clickhouse://host:9000/db"),
// pings it, and validates the expected schema — failing fast on drift (design
// D7) rather than silently returning empty windows. Prefer WithAuth for
// credentials; DSN-embedded userinfo remains supported for backward compatibility.
func Open(ctx context.Context, dsn string, opts ...Option) (*CH, error) {
	var cfg openConfig
	for _, o := range opts {
		o(&cfg)
	}
	chOpts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse route-store dsn: %w", err)
	}
	applyAuth(chOpts, cfg)
	conn, err := clickhouse.Open(chOpts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping route store: %w", err)
	}
	s := &CH{conn: conn, uniqueRows: cfg.uniqueRows}
	if err := s.validateSchema(ctx, chOpts.Auth.Database); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *CH) Close() error { return s.conn.Close() }

// CollapsedRows reports how many rows the client-side dedup has discarded
// over this store's lifetime. Under uniqueRows it is the writer-uniqueness
// alarm: any positive value means the "one row per version" guarantee broke,
// and warnCollapse logs it the first time it happens. Without uniqueRows,
// collapses are expected whenever the query races the exporter's close-rewrite
// or a replay.
func (s *CH) CollapsedRows() uint64 { return s.collapsed.Load() }

// warnCollapse fires the writer-uniqueness alarm the doc above promises. Only
// meaningful under uniqueRows, where a collapse means the assertion the operator
// made when enabling the flag is false — and the documented consequence is
// silent data corruption (the closing rewrite pruned in SQL, a stale sentinel
// twin winning the dedup unopposed, dead config versions treated as live).
//
// Logged once per store: the condition is a property of the writer, not of a
// query, so repeating it per read would bury it. Without uniqueRows a collapse
// is the expected race with the exporter's close-rewrite and says nothing.
func (s *CH) warnCollapse(n int) {
	if !s.uniqueRows {
		return
	}
	s.warnOnce.Do(func() {
		slog.Warn("route store collapsed duplicate rows while --route-store-unique-rows is set: "+
			"the writer's one-row-per-version guarantee is broken, and SQL-side valid_to pruning "+
			"can now treat dead config versions as live — disable the flag or fix the writer",
			"collapsed_rows", n)
	})
}

// dt64Lit renders t as a DateTime64(3) literal in UTC, truncated to the same
// millisecond precision the columns store. Time operands must NOT be `?`
// binds: clickhouse-go interpolates time.Time at second precision
// (toDateTime), which drops milliseconds (a `valid_from <= at` boundary error
// for sub-second windows) and SATURATES on the far-future sentinel
// (2200 > 32-bit DateTime's 2106 ceiling), silently breaking range predicates.
func dt64Lit(t time.Time) string {
	return fmt.Sprintf("toDateTime64('%s', 3, 'UTC')", t.UTC().Format("2006-01-02 15:04:05.000"))
}

// prune returns the optional SQL fragment restoring the valid_to predicate
// ("no version that ended at or before at") under uniqueRows, or "" in the
// superset+dedup mode.
func (s *CH) prune(at time.Time) string {
	if !s.uniqueRows {
		return ""
	}
	return " AND " + dt64Lit(at) + " < valid_to"
}

// versionRow is the dedup/liveness envelope every no-FINAL read scans
// alongside its payload columns: the version-slot identity (cluster,
// namespace, name, valid_from), the writer sequence that picks the
// authoritative row within a slot, and the materialized valid_to that the
// liveness check runs against AFTER dedup.
type versionRow struct {
	cluster, ns, name string
	vf, vt            time.Time
	seq               uint64
}

func (r versionRow) slot() string {
	return r.cluster + "\x00" + r.ns + "\x00" + r.name + "\x00" + strconv.FormatInt(r.vf.UnixMilli(), 10)
}

// liveAt reports vf <= at < vt — the as-of liveness test, checked post-dedup
// (simplify-route-resolution-to-point-in-time D2).
func (r versionRow) liveAt(at time.Time) bool {
	return !r.vf.After(at) && at.Before(r.vt)
}

// dedupLatest keeps, per version slot, only the row with the highest
// ingest_seq — the reader-side equivalent of the ReplacingMergeTree
// (ingest_seq) collapse FINAL would apply server-side. First-seen slot order
// is preserved.
func dedupLatest[T any](rows []T, ver func(T) versionRow) []T {
	if len(rows) < 2 {
		return rows
	}
	idx := make(map[string]int, len(rows))
	out := rows[:0]
	for _, r := range rows {
		k := ver(r).slot()
		if i, seen := idx[k]; seen {
			if ver(r).seq > ver(out[i]).seq {
				out[i] = r
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	return out
}

// dedupLiveAtCounted dedups rows per version slot (collapses counted into
// CollapsedRows — liveness filtering itself is not a collapse) and then keeps
// only the versions live at `at`.
func dedupLiveAtCounted[T any](s *CH, rows []T, ver func(T) versionRow, at time.Time) []T {
	before := len(rows)
	rows = dedupLatest(rows, ver)
	if n := before - len(rows); n > 0 {
		s.collapsed.Add(uint64(n))
		s.warnCollapse(n)
	}
	out := rows[:0]
	for _, r := range rows {
		if ver(r).liveAt(at) {
			out = append(out, r)
		}
	}
	return out
}

// closeRows folds rows.Err + rows.Close into one error.
func closeRows(rows driver.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// pjUnmarshal tolerates unknown fields: production spec_json is the API
// server's CR JSON verbatim, whose field set is decided by the CLUSTER's Istio
// CRD version, not by the istio.io/api version this reader was compiled
// against. A cluster upgrade adding a spec field must not fail the whole
// historical query when the known fields suffice to parse routing (istiod
// itself parses CRs allow-unknown). The accepted trade-off — a discarded field
// that genuinely affects routing silently diverges from what Envoy did — is
// the design §9.4 version-coupling risk.
var pjUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// expectedSchema is the fixed read contract: every column this reader selects,
// per table. The metadata-exporter owns the DDL (types, engine, indexes);
// kube-state-graph only asserts the columns it reads exist. Column names are a
// case-sensitive contract (design.md D26 style).
var expectedSchema = map[string][]string{
	"service_versions": {"cluster", "namespace", "name", "valid_from", "valid_to", "external_ips", "loadbalancer_ips", "selector_kv", "spec_json", "ingest_seq"},
	"deploy_versions":  {"cluster", "namespace", "name", "valid_from", "valid_to", "pod_labels_kv", "ingest_seq"},
	"gw_versions":      {"cluster", "namespace", "name", "valid_from", "valid_to", "selector_kv", "server_hosts", "spec_json", "ingest_seq"},
	"vs_versions":      {"cluster", "namespace", "name", "valid_from", "valid_to", "bound_gateways", "spec_json", "ingest_seq"},
}

// validateSchema asserts every expected (table, column) exists in the connected
// database. One aggregated error names everything missing, so an operator fixes
// the contract in one round trip instead of one column at a time.
func (s *CH) validateSchema(ctx context.Context, database string) error {
	if database == "" {
		database = "default"
	}
	tables := make([]string, 0, len(expectedSchema))
	for t := range expectedSchema {
		tables = append(tables, t)
	}
	rows, err := s.conn.Query(ctx,
		`SELECT table, name FROM system.columns WHERE database = ? AND has(?, table)`,
		database, tables)
	if err != nil {
		return fmt.Errorf("route-store schema check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	have := map[string]map[string]bool{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return err
		}
		if have[table] == nil {
			have[table] = map[string]bool{}
		}
		have[table][col] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var missing []string
	for table, cols := range expectedSchema {
		for _, col := range cols {
			if !have[table][col] {
				missing = append(missing, table+"."+col)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("route-store schema drift: missing %s (database %q); "+
			"the store is written by the metadata-exporter and must carry every column this reader selects",
			strings.Join(missing, ", "), database)
	}
	return nil
}

// svcVer builds the dedup envelope for a ServiceRow.
func svcVer(r ServiceRow) versionRow {
	return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
}

// LoadTrafficAt fetches every resource version live at `at` reachable from
// destination IP in cluster. SQL keeps only the immutable predicates (join
// keys + valid_from <= at, plus the uniqueRows prune); rows are then deduped
// per version slot (max ingest_seq) and the liveness predicate on the
// materialized valid_to (at < valid_to) is applied client-side AFTER dedup —
// the no-FINAL pattern in the CH doc comment.
//
// Each hop loads a correct SUPERSET of what the resolution actually needs
// (e.g. gateways whose selector ⊆ the union of the ingress deployment's pod
// labels across the fetched rows); pkg/route/snapshot re-applies the exact
// predicates, so extra rows are harmless. The hops stay narrowly scoped
// (deploys are still selector-filtered) so the superset does not blow up on a
// shared ingress namespace.
func (s *CH) LoadTrafficAt(ctx context.Context, cluster, ip string, at time.Time) (TrafficSnapshot, error) {
	var w TrafficSnapshot

	// 1. Ingress Service versions serving this IP (either external_ips or
	// loadbalancer_ips — either array counts).
	svcRows, err := s.conn.Query(ctx, fmt.Sprintf(
		`SELECT cluster, namespace, name, valid_from, valid_to, external_ips, loadbalancer_ips, selector_kv, spec_json, ingest_seq
		 FROM service_versions
		 WHERE cluster = ? AND (has(external_ips, ?) OR has(loadbalancer_ips, ?)) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
		cluster, ip, ip)
	if err != nil {
		return w, fmt.Errorf("window ingress services: %w", err)
	}
	var ingSvcs []ServiceRow
	for svcRows.Next() {
		r, err := scanServiceRow(svcRows)
		if err != nil {
			_ = svcRows.Close()
			return w, err
		}
		ingSvcs = append(ingSvcs, r)
	}
	if err := closeRows(svcRows); err != nil {
		return w, fmt.Errorf("window ingress services: %w", err)
	}
	// Dedup BEFORE deriving selectors — a stale rewrite twin must not
	// contribute its (possibly outdated) selector to the hop-2 fan-out.
	ingSvcs = dedupLiveAtCounted(s, ingSvcs, svcVer, at)
	nsSeen := map[string]bool{}
	var nsList []string
	selSeen := map[string]bool{}
	var selectors [][]string // distinct ingress-service selectors across versions
	for _, r := range ingSvcs {
		w.Services = append(w.Services, r)
		if !nsSeen[r.Namespace] {
			nsSeen[r.Namespace] = true
			nsList = append(nsList, r.Namespace)
		}
		// An empty selector selects every pod — skip it, or hop 2 would load
		// every deployment in the (shared) ingress namespace and blow up the
		// label union.
		if k := strings.Join(r.Selector, "\x00"); len(r.Selector) > 0 && !selSeen[k] {
			selSeen[k] = true
			selectors = append(selectors, r.Selector)
		}
	}
	if len(nsList) == 0 || len(selectors) == 0 {
		return w, nil // no ingress serves this IP at that instant -> empty
	}

	// 2. Ingress Deployment versions: those in the ingress namespace(s) whose
	// pod labels ⊇ the ingress service selector. Rows are collected across all
	// selector queries first, then deduped once (the same deploy version can
	// match several selectors).
	var deps []DeployRow
	for _, sel := range selectors {
		depRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, pod_labels_kv, ingest_seq
			 FROM deploy_versions
			 WHERE cluster = ? AND has(?, namespace) AND hasAll(pod_labels_kv, ?) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
			cluster, nsList, sel)
		if err != nil {
			return w, fmt.Errorf("window deploys: %w", err)
		}
		for depRows.Next() {
			var r DeployRow
			if err := depRows.Scan(&r.Cluster, &r.Namespace, &r.Name, &r.ValidFrom, &r.ValidTo, &r.PodLabels, &r.IngestSeq); err != nil {
				_ = depRows.Close()
				return w, err
			}
			deps = append(deps, r)
		}
		if err := closeRows(depRows); err != nil {
			return w, fmt.Errorf("window deploys: %w", err)
		}
	}
	deps = dedupLiveAtCounted(s, deps, func(r DeployRow) versionRow {
		return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
	}, at)
	labelSeen := map[string]bool{}
	var labelUnion []string
	for _, r := range deps {
		w.Deploys = append(w.Deploys, r)
		for _, l := range r.PodLabels {
			if !labelSeen[l] {
				labelSeen[l] = true
				labelUnion = append(labelUnion, l)
			}
		}
	}

	// 3. Gateway versions whose selector ⊆ the pod-label union, scoped to the
	// ingress namespace(s) like the deploy hop: a candidate Gateway must live
	// in the ingress Service's own namespace
	// (scope-gateway-candidates-to-ingress-namespace) — cross-namespace
	// selector attachment is deliberately out of scope. nsList unions all
	// matching ingress Services' namespaces, so this is still a superset;
	// pkg/route/snapshot re-applies the exact per-service-namespace predicate.
	var gwRefs []string
	if len(labelUnion) > 0 {
		gwRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, selector_kv, server_hosts, spec_json, ingest_seq
			 FROM gw_versions
			 WHERE cluster = ? AND has(?, namespace) AND hasAll(?, selector_kv) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
			cluster, nsList, labelUnion)
		if err != nil {
			return w, fmt.Errorf("window gateways: %w", err)
		}
		gwRefs, err = s.appendGateways(&w, gwRows, at)
		if err != nil {
			return w, err
		}
	}

	// 4 + 5. VirtualServices bound to the candidate gateways, then the backend
	// Services those VS route to.
	if err := s.loadVSAndBackends(ctx, &w, cluster, gwRefs, at); err != nil {
		return w, err
	}
	return w, nil
}

// ClustersWithIngressIP is the store's ONLY cross-cluster read (design D10):
// the distinct clusters whose ingress LB Service version carrying ip was live
// at `at`. It deliberately carries no cluster predicate — that is its whole
// job — bounded by the small number of ingress-LB Service versions
// (has(external_ips, ...) OR has(loadbalancer_ips, ...) only matches ingress
// rows) and valid_from <= at. The same no-FINAL pattern as the snapshot loads
// applies: only the dedup envelope is selected, valid_to is never filtered in
// SQL (except under the uniqueRows prune), the client dedups per version slot
// and applies the liveness check after. The result is deduplicated and sorted
// for determinism.
func (s *CH) ClustersWithIngressIP(ctx context.Context, ip string, at time.Time) ([]string, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf(
		`SELECT cluster, namespace, name, valid_from, valid_to, ingest_seq
		 FROM service_versions
		 WHERE (has(external_ips, ?) OR has(loadbalancer_ips, ?)) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
		ip, ip)
	if err != nil {
		return nil, fmt.Errorf("ingress-cluster probe: %w", err)
	}
	var vers []versionRow
	for rows.Next() {
		var r versionRow
		if err := rows.Scan(&r.cluster, &r.ns, &r.name, &r.vf, &r.vt, &r.seq); err != nil {
			_ = rows.Close()
			return nil, err
		}
		vers = append(vers, r)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("ingress-cluster probe: %w", err)
	}
	vers = dedupLiveAtCounted(s, vers, func(r versionRow) versionRow { return r }, at)

	seen := map[string]bool{}
	var clusters []string
	for _, r := range vers {
		if !seen[r.cluster] {
			seen[r.cluster] = true
			clusters = append(clusters, r.cluster)
		}
	}
	sort.Strings(clusters)
	return clusters, nil
}

// appendGateways drains one gw_versions result set, dedups + liveness-filters
// it, appends the survivors to the snapshot, and returns the distinct gateway
// refs for the bound-VS lookup — in BOTH binding forms.
func (s *CH) appendGateways(w *TrafficSnapshot, gwRows driver.Rows, at time.Time) ([]string, error) {
	var gws []GatewayRow
	for gwRows.Next() {
		var r GatewayRow
		if err := gwRows.Scan(&r.Cluster, &r.Namespace, &r.Name, &r.ValidFrom, &r.ValidTo,
			&r.SelectorKV, &r.ServerHosts, &r.SpecJSON, &r.IngestSeq); err != nil {
			_ = gwRows.Close()
			return nil, err
		}
		gws = append(gws, r)
	}
	if err := closeRows(gwRows); err != nil {
		return nil, err
	}
	gws = dedupLiveAtCounted(s, gws, func(r GatewayRow) versionRow {
		return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
	}, at)

	var gwRefs []string
	refSeen := map[string]bool{}
	for _, r := range gws {
		w.Gateways = append(w.Gateways, r)
		// A VS may bind a gateway by qualified "<ns>/<name>" OR by bare
		// "<name>" (legal Istio shorthand for a same-namespace gateway; the
		// exporter stores spec.gateways[*] verbatim, unnormalized). Load the
		// superset by matching both forms — pkg/route/snapshot re-applies
		// the exact predicate (boundTo) with the namespace check.
		for _, ref := range []string{r.Namespace + "/" + r.Name, r.Name} {
			if !refSeen[ref] {
				refSeen[ref] = true
				gwRefs = append(gwRefs, ref)
			}
		}
	}
	return gwRefs, nil
}

// loadVSAndBackends loads the VirtualService versions bound to any candidate
// gateway, then the backend Service versions those VS route to (identity = the
// (namespace, name) parsed from each route's destination.host FQDN). Backend
// lookups are chunked so a gateway with very many routes can't inline a key
// list past ClickHouse's max_query_size.
func (s *CH) loadVSAndBackends(ctx context.Context, w *TrafficSnapshot, cluster string, gwRefs []string, at time.Time) error {
	var destHosts []string
	if len(gwRefs) > 0 {
		vsRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, bound_gateways, spec_json, ingest_seq
			 FROM vs_versions
			 WHERE cluster = ? AND hasAny(bound_gateways, ?) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
			cluster, gwRefs)
		if err != nil {
			return fmt.Errorf("window virtualservices: %w", err)
		}
		var vses []VSRow
		for vsRows.Next() {
			var r VSRow
			if err := vsRows.Scan(&r.Cluster, &r.Namespace, &r.Name, &r.ValidFrom, &r.ValidTo,
				&r.BoundGateways, &r.SpecJSON, &r.IngestSeq); err != nil {
				_ = vsRows.Close()
				return err
			}
			vses = append(vses, r)
		}
		if err := closeRows(vsRows); err != nil {
			return err
		}
		// Dedup BEFORE collecting destination hosts — a stale rewrite twin's
		// routes must not pull in backend Services the live version dropped.
		vses = dedupLiveAtCounted(s, vses, func(r VSRow) versionRow {
			return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
		}, at)
		hostSeen := map[string]bool{}
		for _, r := range vses {
			w.VSes = append(w.VSes, r)
			var vsSpec networking.VirtualService
			if err := pjUnmarshal.Unmarshal([]byte(r.SpecJSON), &vsSpec); err != nil {
				return fmt.Errorf("window unmarshal vs %s/%s: %w", r.Namespace, r.Name, err)
			}
			for _, h := range VSDestHosts(&vsSpec, r.Namespace) {
				if !hostSeen[h] {
					hostSeen[h] = true
					destHosts = append(destHosts, h)
				}
			}
		}
	}

	var backends []ServiceRow
	for _, chunk := range chunkStrings(backendKeys(destHosts), hostChunk) {
		bsRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, external_ips, loadbalancer_ips, selector_kv, spec_json, ingest_seq
			 FROM service_versions
			 WHERE cluster = ? AND has(?, concat(namespace, '/', name)) AND valid_from <= %s%s`, dt64Lit(at), s.prune(at)),
			cluster, chunk)
		if err != nil {
			return fmt.Errorf("window backend services: %w", err)
		}
		for bsRows.Next() {
			r, err := scanServiceRow(bsRows)
			if err != nil {
				_ = bsRows.Close()
				return err
			}
			backends = append(backends, r)
		}
		if err := closeRows(bsRows); err != nil {
			return err
		}
	}
	w.Services = append(w.Services, dedupLiveAtCounted(s, backends, svcVer, at)...)
	return nil
}

// scanServiceRow scans one full service_versions row, decoding spec_json into
// Ports. Shared by the ingress and backend window loads.
func scanServiceRow(rows driver.Rows) (ServiceRow, error) {
	var r ServiceRow
	var specJSON string
	if err := rows.Scan(&r.Cluster, &r.Namespace, &r.Name, &r.ValidFrom, &r.ValidTo,
		&r.ExternalIPs, &r.LoadBalancerIPs, &r.Selector, &specJSON, &r.IngestSeq); err != nil {
		return r, err
	}
	ports, err := ParsePorts(specJSON)
	if err != nil {
		return r, fmt.Errorf("parse ports %s/%s: %w", r.Namespace, r.Name, err)
	}
	r.Ports = ports
	return r, nil
}

// backendKeys maps VS destination.host FQDNs to the "namespace/name" identity
// keys used to look up backend Service rows. Hosts that aren't resolvable
// in-cluster FQDNs are skipped.
func backendKeys(hosts []string) []string {
	keys := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if name, ns, ok := ParseBackendHost(h); ok {
			keys = append(keys, ns+"/"+name)
		}
	}
	return keys
}

// hostChunk bounds how many host FQDNs go into one IN-list, keeping the inlined
// query text well under ClickHouse's default max_query_size (256 KB).
const hostChunk = 2000

// chunkStrings splits s into consecutive slices of at most n (n<=0 => one chunk).
func chunkStrings(s []string, n int) [][]string {
	if n <= 0 || len(s) <= n {
		if len(s) == 0 {
			return nil
		}
		return [][]string{s}
	}
	var out [][]string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}
