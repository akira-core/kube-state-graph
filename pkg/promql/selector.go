package promql

import (
	"regexp"
	"sort"
	"strings"
)

// Default label keys the `az` / `env` request dimensions bind to when the
// deployment does not override them. The REQUEST parameter names (`az`, `env`)
// are fixed and never change with the configured keys — only the upstream
// label a value is matched against moves.
const (
	DefaultAZLabel  = "az"
	DefaultEnvLabel = "env"
)

// clusterUnknownValue is the request value that addresses the "series carries
// no `cluster` label" bucket the topology reader reports as cluster="unknown".
// It renders as the EMPTY-STRING matcher, which PromQL evaluates as "the label
// is absent or empty", so the bucket stays addressable at the query layer.
const clusterUnknownValue = "unknown"

// LabelKeys names the upstream labels the availability-zone and environment
// request dimensions are matched against. The zero value means "use the
// defaults" — call OrDefault (Render does) rather than reading the fields raw.
//
// Keys are validated where they are configured (internal/config), not here:
// Render must stay a pure function, and an embedder constructing LabelKeys
// directly is trusted code.
type LabelKeys struct {
	AZ  string
	Env string
}

// DefaultLabelKeys returns the built-in key binding (`az`, `env`).
func DefaultLabelKeys() LabelKeys { return LabelKeys{AZ: DefaultAZLabel, Env: DefaultEnvLabel} }

// OrDefault returns k with any unset key filled from the built-in defaults.
func (k LabelKeys) OrDefault() LabelKeys {
	if k.AZ == "" {
		k.AZ = DefaultAZLabel
	}
	if k.Env == "" {
		k.Env = DefaultEnvLabel
	}
	return k
}

// Selector carries the request-scoped filter dimensions that are pushed into
// the upstream PromQL queries as label matchers. Each dimension is a set of
// caller-supplied values, OR-combined within a dimension and AND-combined
// across dimensions.
//
// WHICH dimension reaches WHICH series is the hardcoded queryDims contract in
// queries.go — not a property of this value. Notably the three
// traces_service_graph_* queries and the up{} probe accept NO dimension at all,
// and the NetApp Harvest queries accept only AZ / Env (their `cluster` label
// names an ONTAP cluster, not a Kubernetes one).
//
// The zero value is the unfiltered build: every query renders exactly the
// string it rendered before request-scoped selectors existed.
type Selector struct {
	AZ        []string
	Env       []string
	Cluster   []string
	Namespace []string
}

// Active reports whether any dimension carries a NON-EMPTY value — i.e.
// whether this selector will actually render a matcher. It is the single
// "this build is filtered" predicate: the build layer uses it to gate the
// outside-retention classification and to arm the service-graph reader's
// filtered-build admission rules.
//
// Empty values are ignored here for the same reason render ignores them: a
// bare `?namespace=` is a no-op, so it must not silently switch the build into
// filtered mode while every query stays unfiltered.
func (s Selector) Active() bool {
	return hasValue(s.AZ) || hasValue(s.Env) || hasValue(s.Cluster) || hasValue(s.Namespace)
}

func hasValue(values []string) bool {
	for _, v := range values {
		if v != "" {
			return true
		}
	}
	return false
}

// dims is the bitmask of request dimensions a single query accepts.
type dims uint8

const (
	dimAZ dims = 1 << iota
	dimEnv
	dimCluster
	dimNamespace
)

const (
	// dimsNone: request-invariant queries — the service-graph family (whose
	// `cluster` label is the unreliable trace-source cluster and whose
	// namespace labels describe only the caller's own view) and the up{}
	// probe (which measures the store, not the data).
	dimsNone dims = 0
	// dimsHarvest: NetApp Harvest series carry neither a Kubernetes `cluster`
	// nor a `namespace` label; they are narrowed by reference instead (an
	// aggregate materialises only when a loaded claim joins it).
	dimsHarvest = dimAZ | dimEnv
	// dimsClusterScoped: series keyed by cluster but not by namespace
	// (the kube_node_* family).
	dimsClusterScoped = dimAZ | dimEnv | dimCluster
	// dimsNamespaced: pod-, claim-, Service- and EndpointSlice-scoped series
	// plus the kubelet volume-stats family.
	dimsNamespaced = dimAZ | dimEnv | dimCluster | dimNamespace
)

// render returns the request-scoped matcher fragment for a query accepting
// dims d, without surrounding braces. Dimension order is fixed (az, env,
// cluster, namespace) and values are de-duplicated and sorted, so the result
// is a pure function of the value SETS — two requests differing only in
// parameter order render byte-identical queries.
func (s Selector) render(d dims, keys LabelKeys) string {
	if d == 0 {
		return ""
	}
	keys = keys.OrDefault()
	var out []string
	if d&dimAZ != 0 {
		out = appendMatcher(out, keys.AZ, s.AZ, nil)
	}
	if d&dimEnv != 0 {
		out = appendMatcher(out, keys.Env, s.Env, nil)
	}
	if d&dimCluster != 0 {
		out = appendMatcher(out, "cluster", s.Cluster, clusterMatcherValue)
	}
	if d&dimNamespace != 0 {
		out = appendMatcher(out, "namespace", s.Namespace, nil)
	}
	return strings.Join(out, ",")
}

// clusterMatcherValue maps the request value "unknown" onto the empty-string
// matcher so the missing-cluster-label bucket is addressable. Applied AFTER
// sorting, so the rendered order follows the caller's raw values: a
// {alpha, unknown} request renders `cluster=~"alpha|"`.
func clusterMatcherValue(v string) string {
	if v == clusterUnknownValue {
		return ""
	}
	return v
}

// appendMatcher renders one dimension. A single value becomes an exact
// matcher; two or more become ONE fully-anchored alternation (PromQL anchors
// `=~` as ^(?:...)$, so top-level alternation is exactly "any of these").
func appendMatcher(dst []string, key string, values []string, xform func(string) string) []string {
	vals := normaliseValues(values)
	if len(vals) == 0 {
		return dst
	}
	if xform != nil {
		for i := range vals {
			vals[i] = xform(vals[i])
		}
	}
	if len(vals) == 1 {
		return append(dst, key+`="`+escapeLiteral(vals[0])+`"`)
	}
	alts := make([]string, len(vals))
	for i, v := range vals {
		// QuoteMeta first (so a metacharacter matches literally), then string
		// escaping — QuoteMeta emits backslashes, and a PromQL string literal
		// rejects an unknown escape sequence, so `prod.eu` must reach the
		// parser as "prod\\.eu" to unquote into the regex `prod\.eu`.
		alts[i] = escapeLiteral(regexp.QuoteMeta(v))
	}
	return append(dst, key+`=~"`+strings.Join(alts, "|")+`"`)
}

// normaliseValues drops empty values (a bare `?namespace=` is a no-op, matching
// graph.Scope's own set construction), de-duplicates, and sorts.
func normaliseValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// escapeLiteral escapes a value for a double-quoted PromQL string literal.
// Control characters are rejected by the request parser before they reach
// here, so backslash and double quote are the whole set.
func escapeLiteral(s string) string {
	if !strings.ContainsAny(s, "\\\"") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// Reaches reports whether any dimension this selector actually carries is one
// that query q accepts — i.e. whether q's result set could have been narrowed
// by THIS request. It is the queryDims table read from the other direction, so
// the build layer can attribute an empty metric family to the request instead
// of to the deployment without keeping its own copy of the contract.
func (s Selector) Reaches(q Query) bool {
	d := queryDims[q]
	return (d&dimAZ != 0 && hasValue(s.AZ)) ||
		(d&dimEnv != 0 && hasValue(s.Env)) ||
		(d&dimCluster != 0 && hasValue(s.Cluster)) ||
		(d&dimNamespace != 0 && hasValue(s.Namespace))
}
