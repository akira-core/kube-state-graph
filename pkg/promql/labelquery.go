package promql

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/common/model"
)

// DefaultLabelQueryLimit is the number of label sets QueryLabels returns when
// the request does not set Limit. Exceeding it is an error, never a truncation.
const DefaultLabelQueryLimit = 10000

// MaxLabelQueryValueLen is the maximum accepted length, in bytes, of the AZ
// value and of every label-filter value. Longer values fail the request
// before any upstream call.
const MaxLabelQueryValueLen = 1024

// QueryNameLabelQuery is the fixed `query` label recorded on
// kube_state_graph_upstream_query_duration_seconds and
// kube_state_graph_upstream_query_failures_total for every QueryLabels call.
// Passing the caller's metric name would make an unbounded label set out of a
// bounded one; the metric name still reaches the Debug log and the span's
// db.statement.
const QueryNameLabelQuery = "label_query"

// metricNameRe is the Prometheus metric-name grammar: a letter, underscore or
// colon, then letters, digits, underscores or colons. Label names are the
// same minus the colon.
var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// LabelQuery is the embedder-facing request for Router.QueryLabels: one
// arbitrary metric, one declared family, optional single zone, and a set of
// exact label equalities. Sample values are never returned.
//
// Family is required and must be one of the six declared families. AZ is the
// only named dimension because it decides which store is asked; every other
// label is a filter. At is required — pkg/promql holds no clock, and a zero
// instant is a caller bug rather than "now".
type LabelQuery struct {
	Metric    string
	Family    Family
	AZ        string
	Filters   map[string]string
	At        time.Time
	Window    time.Duration
	Limit     int
	LabelKeys LabelKeys
}

// QueryLabels selects the backends serving req.Family (narrowed to req.AZ
// when the family is zone-routed), issues one identical query to each, merges
// and de-duplicates the results, and returns each matched series' label set.
//
// The family is the caller's, not derived from a query-name table: that is
// what lets an arbitrary metric reach the right store. Dispatch itself is
// the same core Instant uses (zone selection, identical-string fan-out,
// fingerprint merge, fail-closed). An unserved family is an error here —
// unlike the server's optional alerts leg, this call is deliberate.
func (r *Router) QueryLabels(ctx context.Context, req LabelQuery) ([]map[string]string, error) {
	fam, err := req.validate()
	if err != nil {
		return nil, err
	}
	// Dispatch, selection and rendering all read the PARSED family, never the
	// caller's raw value, so the three can never disagree about which store is
	// asked and which matcher is rendered.
	req.Family = fam

	st := r.state.Load()
	if len(st.table.Select(fam, nil)) == 0 {
		return nil, fmt.Errorf("prom label query: family %q is served by no backend", fam)
	}

	var az []string
	if req.AZ != "" {
		az = []string{req.AZ}
	}
	vec, err := r.querier(st, az).issue(ctx, fam, QueryNameLabelQuery, req.render(), req.At)
	if err != nil {
		return nil, err
	}

	// De-duplication happens here rather than in the vector merge: that merge
	// keys on the full label set INCLUDING __name__, and whether an upstream
	// preserves __name__ through a rolling function varies by engine (D7). Two
	// backends of different engines holding one series would otherwise survive
	// the merge as two fingerprints and land in the result twice.
	entries := labelSetsOf(vec)
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLabelQueryLimit
	}
	if len(entries) > limit {
		return nil, fmt.Errorf("prom label query: result count %d exceeds limit %d", len(entries), limit)
	}
	sortLabelSets(entries)

	out := make([]map[string]string, len(entries))
	for i, e := range entries {
		out[i] = e.labels
	}
	return out, nil
}

// validate rejects a malformed request before any string reaches a query, and
// returns the parsed family so the caller renders and routes on the same value.
func (q LabelQuery) validate() (Family, error) {
	fam, ok := ParseFamily(string(q.Family))
	if !ok {
		return "", fmt.Errorf("prom label query: unknown family %q", q.Family)
	}

	if !metricNameRe.MatchString(q.Metric) {
		return "", fmt.Errorf("prom label query: invalid metric name %q", q.Metric)
	}

	keys := q.LabelKeys.OrDefault()
	// The az key is caller-supplied and is rendered into the query string, so
	// it is validated exactly like a filter key. Without this a binding such as
	// `az",foo="bar` would introduce a second matcher.
	if !labelNameRe.MatchString(keys.AZ) {
		return "", fmt.Errorf("prom label query: invalid az label key %q", keys.AZ)
	}
	for k := range q.Filters {
		if k == model.MetricNameLabel {
			return "", fmt.Errorf("prom label query: filter key %q is reserved; name the metric in Metric", k)
		}
		if !labelNameRe.MatchString(k) {
			return "", fmt.Errorf("prom label query: invalid label filter key %q", k)
		}
		if q.AZ != "" && k == keys.AZ {
			return "", fmt.Errorf("prom label query: filter key %q conflicts with the AZ field; drop the filter or leave AZ empty", k)
		}
	}

	if q.AZ != "" {
		if err := validateLabelQueryValue("AZ value", q.AZ); err != nil {
			return "", err
		}
	}
	for k, v := range q.Filters {
		if err := validateLabelQueryValue(fmt.Sprintf("filter %q value", k), v); err != nil {
			return "", err
		}
	}

	if q.At.IsZero() {
		return "", fmt.Errorf("prom label query: evaluation instant is required")
	}

	if q.AZ != "" && !fam.AcceptsAZ() {
		return "", fmt.Errorf("prom label query: family %q does not route by zone; leave AZ empty and pass the az label as a filter", fam)
	}
	return fam, nil
}

func validateLabelQueryValue(what, val string) error {
	if len(val) > MaxLabelQueryValueLen {
		return fmt.Errorf("prom label query: %s exceeds %d bytes", what, MaxLabelQueryValueLen)
	}
	if !utf8.ValidString(val) {
		return fmt.Errorf("prom label query: %s is not valid UTF-8", what)
	}
	for _, r := range val {
		if unicode.IsControl(r) {
			return fmt.Errorf("prom label query: %s contains a control character", what)
		}
	}
	return nil
}

// render builds the PromQL selector. Matcher order is the family-derived az
// matcher (when rendered) then filters sorted by key, so two requests that
// differ only in map iteration order produce one string. Values go through
// escapeLiteral; validation has already rejected control characters and
// invalid UTF-8.
func (q LabelQuery) render() string {
	keys := q.LabelKeys.OrDefault()
	n := len(q.Filters)
	if q.AZ != "" && q.Family.RendersAZ() {
		n++
	}
	matchers := make([]string, 0, n)
	if q.AZ != "" && q.Family.RendersAZ() {
		matchers = append(matchers, keys.AZ+`="`+escapeLiteral(q.AZ)+`"`)
	}
	for _, k := range slices.Sorted(maps.Keys(q.Filters)) {
		matchers = append(matchers, k+`="`+escapeLiteral(q.Filters[k])+`"`)
	}

	selector := q.Metric
	if len(matchers) > 0 {
		selector += "{" + strings.Join(matchers, ",") + "}"
	}
	if q.Window > 0 {
		return "last_over_time(" + selector + "[" + FormatDuration(q.Window) + "])"
	}
	return selector
}

// labelSetEntry is one matched series decorated with the two things ordering
// and de-duplication need: the label names sorted once (rather than re-sorted
// on every comparison), and an exact identity string.
type labelSetEntry struct {
	labels map[string]string
	keys   []string
	id     string
}

// labelSetsOf converts the merged vector into label sets, stripping the
// reserved metric-name label and dropping a set already contributed.
func labelSetsOf(vec model.Vector) []labelSetEntry {
	out := make([]labelSetEntry, 0, len(vec))
	seen := make(map[string]struct{}, len(vec))
	for _, s := range vec {
		if s == nil {
			continue
		}
		m := make(map[string]string, len(s.Metric))
		for k, v := range s.Metric {
			if k == model.MetricNameLabel {
				continue
			}
			m[string(k)] = string(v)
		}
		keys := slices.Sorted(maps.Keys(m))
		id := labelSetID(m, keys)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, labelSetEntry{labels: m, keys: keys, id: id})
	}
	return out
}

// labelSetID encodes a label set as a collision-free string. Every name and
// value is length-prefixed, so no separator can be forged by a label value —
// two sets share an id iff they are the same map.
func labelSetID(m map[string]string, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		v := m[k]
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte(':')
		b.WriteString(k)
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}

func sortLabelSets(sets []labelSetEntry) {
	slices.SortStableFunc(sets, compareLabelSets)
}

func compareLabelSets(a, b labelSetEntry) int {
	n := min(len(a.keys), len(b.keys))
	for i := range n {
		if c := cmp.Compare(a.keys[i], b.keys[i]); c != 0 {
			return c
		}
		k := a.keys[i]
		if c := cmp.Compare(a.labels[k], b.labels[k]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.keys), len(b.keys))
}
