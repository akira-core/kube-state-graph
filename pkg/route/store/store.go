// Package store is the READ-ONLY contract for the versioned Istio-config store
// backing route resolution (the translate-global-fqdn-to-k8s-service change).
// The metadata-exporter repo owns watch/ingest and the schema; kube-state-graph
// only loads version windows out of it — there is deliberately no CreateSchema
// and no inserter here (design D7). Ported from poc/route2a/internal/store with
// one structural change: every row carries a `cluster` column and every window
// query is scoped by it, because kube-state-graph is multi-cluster and a
// Gateway/VS lookup must never leak across clusters. The one deliberate
// cross-cluster read is ClustersWithIngressIP, the ingress-cluster selection
// probe (design D10).
package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ServiceRow is one service_versions row (ingress LB: IngressIPs/Selector set,
// Ports empty; backend: Ports set, IngressIPs empty). One row per Service
// version — a backend Service's ports all live in Ports (decoded from the
// spec_json column), so a multi-port Service is a single row, not one row per
// port. The backend Service's identity is (Cluster, Namespace, Name); its FQDN
// (the destination.host a VS routes to) is derived — see BackendFQDN /
// ParseBackendHost.
//
// No Rev field on any row type: `rev` was the POC corpus's synthetic oracle
// column. The production tables the exporter writes identify versions by
// resource_version + ingest_seq envelope columns instead, resolution never
// needed a rev, and selecting it against production would fail with
// Unknown column.
type ServiceRow struct {
	Cluster              string
	Namespace, Name      string
	ValidFrom, ValidTo   time.Time
	IngressIPs, Selector []string
	Ports                []SvcPort
	IngestSeq            uint64
}

// SvcPort is one Service port. JSON tags match the corev1 ServiceSpec.ports
// shape so the reader unmarshals the exporter's `spec` blob directly (extra
// fields ignored).
type SvcPort struct {
	Name     string `json:"name"`
	Port     uint32 `json:"port"`
	Protocol string `json:"protocol"`
}

// serviceDomain is the standard Kubernetes Service FQDN suffix. The reader
// assumes the default cluster domain; the exporter stores only (namespace,
// name), so the FQDN mapping is Istio-side derivation that lives here.
const serviceDomain = ".svc.cluster.local"

// BackendFQDN reconstructs a backend Service's FQDN (its destination.host
// identity) from its (name, namespace) — the inverse of ParseBackendHost.
func BackendFQDN(name, ns string) string { return name + "." + ns + serviceDomain }

// ParseBackendHost derives (name, namespace) from a VirtualService route's
// destination.host FQDN (name.namespace.svc.cluster.local). ok=false for a host
// that isn't a resolvable in-cluster FQDN (e.g. a bare short name or external
// host), which the caller skips.
func ParseBackendHost(host string) (name, ns string, ok bool) {
	rest, found := strings.CutSuffix(host, serviceDomain)
	if !found {
		return "", "", false
	}
	labels := strings.SplitN(rest, ".", 3)
	if len(labels) < 2 || labels[0] == "" || labels[1] == "" {
		return "", "", false
	}
	return labels[0], labels[1], true
}

// serviceSpec is the minimal shape read out of the spec_json column: just the
// ports subtree of a Service spec. It mirrors the field the exporter's `spec`
// blob exposes, so one decoder works for both the test corpus and production.
type serviceSpec struct {
	Ports []SvcPort `json:"ports"`
}

// MarshalPorts encodes a Service's ports as a spec_json column value
// (`{"ports":[...]}`), or "" for a portless (ingress LB) row. The reader never
// writes production rows — this exists for test corpora that seed a store.
func MarshalPorts(ports []SvcPort) (string, error) {
	if len(ports) == 0 {
		return "", nil
	}
	b, err := json.Marshal(serviceSpec{Ports: ports})
	return string(b), err
}

// ParsePorts decodes the ports out of a spec_json column value ("" => none).
// Unknown fields are ignored, so it reads a full exporter `spec` blob.
func ParsePorts(specJSON string) ([]SvcPort, error) {
	if specJSON == "" {
		return nil, nil
	}
	var s serviceSpec
	if err := json.Unmarshal([]byte(specJSON), &s); err != nil {
		return nil, err
	}
	return s.Ports, nil
}

// GatewayCand is one candidate gateway from the IP 3-hop (name + server host
// patterns, enough to build a gwresolve.Resolver).
type GatewayCand struct {
	Namespace   string
	Name        string
	ServerHosts []string
}

// DeployRow / GatewayRow / VSRow are full version rows for the range path. They
// carry the materialized ValidFrom/ValidTo so the in-memory window can do AsOf
// without re-hitting the DB; GatewayRow/VSRow keep spec_json so ScopedFor can
// rebuild translate input off the loaded rows.
type DeployRow struct {
	Cluster            string
	Namespace, Name    string
	ValidFrom, ValidTo time.Time
	PodLabels          []string
	IngestSeq          uint64
}

type GatewayRow struct {
	Cluster            string
	Namespace, Name    string
	ValidFrom, ValidTo time.Time
	SelectorKV         []string
	ServerHosts        []string
	SpecJSON           string
	IngestSeq          uint64
}

type VSRow struct {
	Cluster            string
	Namespace, Name    string
	ValidFrom, ValidTo time.Time
	BoundGateways      []string
	SpecJSON           string
	IngestSeq          uint64
}

// TrafficWindow is every resource version overlapping [t0,t1) that one load
// pulled: the ingress Service, its Deployment, the candidate Gateways, their
// bound VirtualServices, and the backend Services those VS route to. It is
// loaded once (LoadTrafficWindow), then sliced/resolved in memory
// (pkg/route/memwindow) with no further DB round-trips. Each row's ValidTo is
// the materialized column value, so AsOf(t) is `ValidFrom <= t < ValidTo`.
type TrafficWindow struct {
	Services []ServiceRow // ingress LB + backend destination Service versions
	Deploys  []DeployRow
	Gateways []GatewayRow
	VSes     []VSRow
}

// Store is the read-only window loader one backend implements. The window
// load is scoped to a single cluster — route resolution must never mix one
// cluster's Gateways with another's. The ONLY cross-cluster read is the
// ingress-IP probe that feeds ingress-cluster selection (design D10).
type Store interface {
	Close() error

	// LoadTrafficWindow fetches every resource version overlapping [t0,t1)
	// that is reachable from destination IP in cluster (one scoped Overlap
	// load per resource kind, using the materialized valid_to:
	// valid_from < t1 AND t0 < valid_to). The caller slices and resolves the
	// returned window in memory.
	LoadTrafficWindow(ctx context.Context, cluster, ip string, t0, t1 time.Time) (TrafficWindow, error)

	// ClustersWithIngressIP is the store's ONLY cross-cluster read: the
	// distinct clusters that had an ingress LB Service version carrying ip
	// overlapping [t0,t1). It feeds the ingress-cluster selection (design
	// D10) — ClickHouse's whole job there is answering "ip → []cluster". The
	// same no-FINAL semantics as the window load apply (valid_to never
	// filtered in SQL except under the uniqueRows prune; client-side dedup
	// per version slot by max ingest_seq; overlap checked post-dedup). The
	// result is deduplicated and sorted for determinism.
	ClustersWithIngressIP(ctx context.Context, ip string, t0, t1 time.Time) ([]string, error)
}
