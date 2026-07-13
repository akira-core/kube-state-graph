package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
//  3. The Overlap predicate on valid_to (`t0 < valid_to`) is applied AFTER
//     dedup, in Go (dedupOverlapCounted).
//
// Trade-off: step 1 fetches every version of the scoped keys with
// valid_from < t1, including versions that ended before the window — bounded
// by per-key version counts (and table TTL), still scoped by the same join
// predicates. In exchange, scans run at plain-MergeTree speed with skip
// indexes fully effective.
//
// # Pruned mode (WithUniqueRows — update-close writers only)
//
// When the writer guarantees one physical row per version (exporter
// closeMode=update, after historical convergence), open the store with
// WithUniqueRows: every query then restores the valid_to predicate in SQL, so
// versions that ended at or before the window are pruned server-side instead
// of fetched. Steps 2 and 3 stay in place as a zero-cost safety net;
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
}

var _ Store = (*CH)(nil)

// Option tweaks a CH store at Open time.
type Option func(*CH)

// WithUniqueRows enables the pruned read mode for update-close writers ONLY.
// See the CH doc comment for the hazard against rewrite-close writers.
func WithUniqueRows() Option { return func(s *CH) { s.uniqueRows = true } }

// Open dials ClickHouse from a DSN (e.g. "clickhouse://user:pass@host:9000/db"),
// pings it, and validates the expected schema — failing fast on drift (design
// D7) rather than silently returning empty windows.
func Open(ctx context.Context, dsn string, opts ...Option) (*CH, error) {
	chOpts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse route-store dsn: %w", err)
	}
	conn, err := clickhouse.Open(chOpts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping route store: %w", err)
	}
	s := &CH{conn: conn}
	for _, o := range opts {
		o(s)
	}
	if err := s.validateSchema(ctx, chOpts.Auth.Database); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *CH) Close() error { return s.conn.Close() }

// CollapsedRows reports how many rows the client-side dedup has discarded
// over this store's lifetime. Under uniqueRows it is the writer-uniqueness
// alarm: any positive value means the "one row per version" guarantee broke
// (wire to a metric/alert in production — tracked as follow-up). Without
// uniqueRows, collapses are expected whenever the query races the exporter's
// close-rewrite or a replay.
func (s *CH) CollapsedRows() uint64 { return s.collapsed.Load() }

// dt64Lit renders t as a DateTime64(3) literal in UTC, truncated to the same
// millisecond precision the columns store. Time operands must NOT be `?`
// binds: clickhouse-go interpolates time.Time at second precision
// (toDateTime), which drops milliseconds (a `valid_from < t1` boundary error
// for sub-second windows) and SATURATES on the far-future sentinel
// (2200 > 32-bit DateTime's 2106 ceiling), silently breaking range predicates.
func dt64Lit(t time.Time) string {
	return fmt.Sprintf("toDateTime64('%s', 3, 'UTC')", t.UTC().Format("2006-01-02 15:04:05.000"))
}

// prune returns the optional SQL fragment restoring the valid_to predicate
// ("no version that ended at or before t") under uniqueRows, or "" in the
// superset+dedup mode.
func (s *CH) prune(t time.Time) string {
	if !s.uniqueRows {
		return ""
	}
	return " AND " + dt64Lit(t) + " < valid_to"
}

// versionRow is the dedup/liveness envelope every no-FINAL read scans
// alongside its payload columns: the version-slot identity (cluster,
// namespace, name, valid_from), the writer sequence that picks the
// authoritative row within a slot, and the materialized valid_to that the
// overlap check runs against AFTER dedup.
type versionRow struct {
	cluster, ns, name string
	vf, vt            time.Time
	seq               uint64
}

func (r versionRow) slot() string {
	return r.cluster + "\x00" + r.ns + "\x00" + r.name + "\x00" + strconv.FormatInt(r.vf.UnixMilli(), 10)
}

// overlapsWindow reports vf < t1 && t0 < vt (checked post-dedup).
func (r versionRow) overlapsWindow(t0, t1 time.Time) bool {
	return r.vf.Before(t1) && t0.Before(r.vt)
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

// dedupOverlapCounted dedups rows per version slot (collapses counted into
// CollapsedRows — overlap filtering itself is not a collapse) and then keeps
// only the versions overlapping [t0,t1).
func dedupOverlapCounted[T any](s *CH, rows []T, ver func(T) versionRow, t0, t1 time.Time) []T {
	before := len(rows)
	rows = dedupLatest(rows, ver)
	if n := before - len(rows); n > 0 {
		s.collapsed.Add(uint64(n))
	}
	out := rows[:0]
	for _, r := range rows {
		if ver(r).overlapsWindow(t0, t1) {
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
	"service_versions": {"cluster", "namespace", "name", "valid_from", "valid_to", "ingress_ips", "selector_kv", "spec_json", "ingest_seq"},
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

// LoadTrafficWindow fetches every resource version overlapping [t0,t1)
// reachable from destination IP in cluster. SQL keeps only the immutable
// predicates (join keys + valid_from < t1, plus the uniqueRows prune); rows
// are then deduped per version slot (max ingest_seq) and the Overlap predicate
// on the materialized valid_to (t0 < valid_to) is applied client-side AFTER
// dedup — the no-FINAL pattern in the CH doc comment.
//
// Each hop loads a correct SUPERSET of what any single-instant point query in
// the window would touch (e.g. gateways whose selector ⊆ the union of the
// ingress deployment's pod labels across versions); memwindow re-applies the
// exact per-instant predicate, so extra rows are harmless. The hops stay
// scoped like a point query (deploys are still selector-filtered) so the
// superset does not blow up on a shared ingress namespace.
func (s *CH) LoadTrafficWindow(ctx context.Context, cluster, ip string, t0, t1 time.Time) (TrafficWindow, error) {
	var w TrafficWindow

	// 1. Ingress Service versions serving this IP.
	svcRows, err := s.conn.Query(ctx, fmt.Sprintf(
		`SELECT cluster, namespace, name, valid_from, valid_to, ingress_ips, selector_kv, spec_json, ingest_seq
		 FROM service_versions
		 WHERE cluster = ? AND has(ingress_ips, ?) AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
		cluster, ip)
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
	ingSvcs = dedupOverlapCounted(s, ingSvcs, svcVer, t0, t1)
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
		return w, nil // no ingress serves this IP in the window -> empty
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
			 WHERE cluster = ? AND has(?, namespace) AND hasAll(pod_labels_kv, ?) AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
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
	deps = dedupOverlapCounted(s, deps, func(r DeployRow) versionRow {
		return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
	}, t0, t1)
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

	// 3. Gateway versions whose selector ⊆ the pod-label union.
	var gwRefs []string
	if len(labelUnion) > 0 {
		gwRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, selector_kv, server_hosts, spec_json, ingest_seq
			 FROM gw_versions
			 WHERE cluster = ? AND hasAll(?, selector_kv) AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
			cluster, labelUnion)
		if err != nil {
			return w, fmt.Errorf("window gateways: %w", err)
		}
		gwRefs, err = s.appendGateways(&w, gwRows, t0, t1)
		if err != nil {
			return w, err
		}
	}

	// 4 + 5. VirtualServices bound to the candidate gateways, then the backend
	// Services those VS route to. Shared with LoadConfigWindow.
	if err := s.loadVSAndBackends(ctx, &w, cluster, gwRefs, t0, t1); err != nil {
		return w, err
	}
	return w, nil
}

// LoadConfigWindow is the IP-less window for config_only mode: every Gateway
// version of the cluster overlapping [t0,t1), their bound VirtualServices, and
// the backend Services those VS route to. No ingress Service or Deployment rows
// are loaded — the config-only path never runs the IP 3-hop.
func (s *CH) LoadConfigWindow(ctx context.Context, cluster string, t0, t1 time.Time) (TrafficWindow, error) {
	var w TrafficWindow

	gwRows, err := s.conn.Query(ctx, fmt.Sprintf(
		`SELECT cluster, namespace, name, valid_from, valid_to, selector_kv, server_hosts, spec_json, ingest_seq
		 FROM gw_versions
		 WHERE cluster = ? AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
		cluster)
	if err != nil {
		return w, fmt.Errorf("config window gateways: %w", err)
	}
	gwRefs, err := s.appendGateways(&w, gwRows, t0, t1)
	if err != nil {
		return w, err
	}

	if err := s.loadVSAndBackends(ctx, &w, cluster, gwRefs, t0, t1); err != nil {
		return w, err
	}
	return w, nil
}

// appendGateways drains one gw_versions result set, dedups + overlap-filters
// it, appends the survivors to the window, and returns the distinct gateway
// refs for the bound-VS lookup — in BOTH binding forms.
func (s *CH) appendGateways(w *TrafficWindow, gwRows driver.Rows, t0, t1 time.Time) ([]string, error) {
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
	gws = dedupOverlapCounted(s, gws, func(r GatewayRow) versionRow {
		return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
	}, t0, t1)

	var gwRefs []string
	refSeen := map[string]bool{}
	for _, r := range gws {
		w.Gateways = append(w.Gateways, r)
		// A VS may bind a gateway by qualified "<ns>/<name>" OR by bare
		// "<name>" (legal Istio shorthand for a same-namespace gateway; the
		// exporter stores spec.gateways[*] verbatim, unnormalized). Load the
		// superset by matching both forms — memwindow re-applies the exact
		// per-instant predicate (boundTo) with the namespace check.
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
func (s *CH) loadVSAndBackends(ctx context.Context, w *TrafficWindow, cluster string, gwRefs []string, t0, t1 time.Time) error {
	var destHosts []string
	if len(gwRefs) > 0 {
		vsRows, err := s.conn.Query(ctx, fmt.Sprintf(
			`SELECT cluster, namespace, name, valid_from, valid_to, bound_gateways, spec_json, ingest_seq
			 FROM vs_versions
			 WHERE cluster = ? AND hasAny(bound_gateways, ?) AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
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
		vses = dedupOverlapCounted(s, vses, func(r VSRow) versionRow {
			return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
		}, t0, t1)
		hostSeen := map[string]bool{}
		for _, r := range vses {
			w.VSes = append(w.VSes, r)
			var vsSpec networking.VirtualService
			if err := pjUnmarshal.Unmarshal([]byte(r.SpecJSON), &vsSpec); err != nil {
				return fmt.Errorf("window unmarshal vs %s/%s: %w", r.Namespace, r.Name, err)
			}
			for _, h := range vsDestHosts(&vsSpec) {
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
			`SELECT cluster, namespace, name, valid_from, valid_to, ingress_ips, selector_kv, spec_json, ingest_seq
			 FROM service_versions
			 WHERE cluster = ? AND has(?, concat(namespace, '/', name)) AND valid_from < %s%s`, dt64Lit(t1), s.prune(t0)),
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
	w.Services = append(w.Services, dedupOverlapCounted(s, backends, svcVer, t0, t1)...)
	return nil
}

// scanServiceRow scans one full service_versions row, decoding spec_json into
// Ports. Shared by the ingress and backend window loads.
func scanServiceRow(rows driver.Rows) (ServiceRow, error) {
	var r ServiceRow
	var specJSON string
	if err := rows.Scan(&r.Cluster, &r.Namespace, &r.Name, &r.ValidFrom, &r.ValidTo,
		&r.IngressIPs, &r.Selector, &specJSON, &r.IngestSeq); err != nil {
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

// vsDestHosts collects the destination host (target Service identity FQDN) of
// every route in a VirtualService (HTTP/TLS/TCP, mirrors included). Kept in
// sync with memwindow's copy.
func vsDestHosts(vs *networking.VirtualService) []string {
	var hosts []string
	add := func(d *networking.Destination) {
		if d != nil && d.GetHost() != "" {
			hosts = append(hosts, d.GetHost())
		}
	}
	for _, r := range vs.GetHttp() {
		for _, rd := range r.GetRoute() {
			add(rd.GetDestination())
		}
		if m := r.GetMirror(); m != nil {
			add(m)
		}
	}
	for _, r := range vs.GetTls() {
		for _, rd := range r.GetRoute() {
			add(rd.GetDestination())
		}
	}
	for _, r := range vs.GetTcp() {
		for _, rd := range r.GetRoute() {
			add(rd.GetDestination())
		}
	}
	return hosts
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
