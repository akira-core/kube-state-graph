// Package promqlfake provides an in-memory promql.Querier for pkg/ tests that
// need the upstream's label-matcher semantics rather than a by-name fixture.
// Every query is answered from a per-family fixture filtered through the
// matchers of the rendered selector, the way VictoriaMetrics would filter it,
// so a test can prove that narrowing a query changes what is fetched without
// changing what is built. Living under pkg/internal it is importable by every
// pkg/... test file while remaining invisible to external embedders.
package promqlfake

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Issued is one query the fake answered.
type Issued struct {
	Name  string
	Query string
}

// Querier answers Instant from Fixtures, applying the query's label matchers.
// A query naming a family with no fixture answers an empty vector. The zero
// value is not usable; construct one with New.
type Querier struct {
	// Fail, when non-nil, is consulted before every query; a non-nil result is
	// returned as the query's error.
	Fail func(name, query string) error

	fixtures map[promql.Query]model.Vector

	mu     sync.Mutex
	issued []Issued
}

// New returns a Querier over fixtures, keyed by query name.
func New(fixtures map[promql.Query]model.Vector) *Querier {
	return &Querier{fixtures: fixtures}
}

// Instant implements promql.Querier.
func (f *Querier) Instant(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
	f.mu.Lock()
	f.issued = append(f.issued, Issued{Name: name, Query: query})
	f.mu.Unlock()

	if f.Fail != nil {
		if err := f.Fail(name, query); err != nil {
			return nil, err
		}
	}
	matchers, err := parseSelector(query)
	if err != nil {
		return nil, fmt.Errorf("promqlfake: %s: %w", name, err)
	}
	out := model.Vector{}
	for _, s := range f.fixtures[promql.Query(name)] {
		if matchesAll(s.Metric, matchers) {
			out = append(out, &model.Sample{Metric: s.Metric.Clone(), Value: s.Value, Timestamp: s.Timestamp})
		}
	}
	return out, nil
}

// Issued returns every query answered so far, in arrival order.
func (f *Querier) Issued() []Issued {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Issued(nil), f.issued...)
}

// QueriesFor returns the rendered queries issued under one family name.
func (f *Querier) QueriesFor(name promql.Query) []string {
	var out []string
	for _, is := range f.Issued() {
		if is.Name == string(name) {
			out = append(out, is.Query)
		}
	}
	return out
}

// ScopeValues returns, for every query issued under family name, the value
// set the query's matcher on label named — one entry per issued query (so a
// chunked scope reports one entry per chunk, in issue order). An exact-equality
// matcher (`label="v"`) reports a single-element set; an alternation
// (`label=~"a|b"`) reports one element per alternative, in the rendered order
// (RenderScoped sorts values before rendering, so this is the sorted set). A
// query carrying no matcher on label contributes no entry. Lets a test assert
// "restricted to exactly {…}" on the value set a data-derived scope rendered,
// rather than string-matching the whole query.
func (f *Querier) ScopeValues(name promql.Query, label string) [][]string {
	var out [][]string
	for _, q := range f.QueriesFor(name) {
		matchers, err := parseSelector(q)
		if err != nil {
			continue
		}
		for _, m := range matchers {
			if m.name != label {
				continue
			}
			switch m.op {
			case "=":
				out = append(out, []string{m.val})
			case "=~":
				// RenderScoped QuoteMeta-escapes every alternative, so a value
				// like `ip-10-0-0-1.ec2.internal` arrives as `ip-10-0-0-1\.ec2\.internal`.
				// Split on UNESCAPED separators and drop the escapes so the entry
				// is the literal value set that was rendered.
				//
				// A tracking-id restriction wraps the alternation as
				// `(?:a|b)(?::.*)?`. Unwrap that group back to the Application
				// names; every other regex keeps the existing split.
				if vals, ok := unwrapTrackingIDGroup(m.val); ok {
					out = append(out, vals)
					continue
				}
				out = append(out, splitQuotedAlternation(m.val))
			}
		}
	}
	return out
}

// splitQuotedAlternation inverts the `regexp.QuoteMeta` + `|`-join a scoped
// alternation is rendered with: it splits on every `|` not preceded by an
// escaping backslash and removes the backslash from each `\x` escape.
func splitQuotedAlternation(re string) []string {
	var (
		out []string
		cur strings.Builder
	)
	for i := 0; i < len(re); i++ {
		switch c := re[i]; {
		case c == '\\' && i+1 < len(re):
			i++
			cur.WriteByte(re[i])
		case c == '|':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// unwrapTrackingIDGroup inverts the `(?:a|b)(?::.*)?` wrapper
// RenderTrackingIDScoped puts around an Application alternation. ok is false
// when re is not that shape.
func unwrapTrackingIDGroup(re string) ([]string, bool) {
	const prefix, suffix = "(?:", ")(?::.*)?"
	if !strings.HasPrefix(re, prefix) || !strings.HasSuffix(re, suffix) {
		return nil, false
	}
	return splitQuotedAlternation(re[len(prefix) : len(re)-len(suffix)]), true
}

type matcher struct {
	name string
	op   string
	val  string
	re   *regexp.Regexp
}

func (m matcher) matches(metric model.Metric) bool {
	v := string(metric[model.LabelName(m.name)]) // an absent label is ""
	switch m.op {
	case "=":
		return v == m.val
	case "!=":
		return v != m.val
	case "=~":
		return m.re.MatchString(v)
	default: // "!~"
		return !m.re.MatchString(v)
	}
}

func matchesAll(metric model.Metric, ms []matcher) bool {
	for _, m := range ms {
		if !m.matches(metric) {
			return false
		}
	}
	return true
}

// parseSelector reads the matchers of the first `{...}` in a rendered query.
// It understands exactly the shapes pkg/promql renders — `name op "literal"`,
// comma-separated, with op one of = != =~ !~ — and PromQL's full anchoring of
// regex matchers. A query with no braces has no matchers.
func parseSelector(query string) ([]matcher, error) {
	open := strings.IndexByte(query, '{')
	if open < 0 {
		return nil, nil
	}
	s := query[open+1:]
	var out []matcher
	for {
		s = strings.TrimLeft(s, " ,")
		if strings.HasPrefix(s, "}") {
			return out, nil
		}
		i := 0
		for i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
			i++
		}
		if i == 0 {
			return nil, fmt.Errorf("label name expected at %q", s)
		}
		m := matcher{name: s[:i]}
		s = s[i:]
		for _, op := range []string{"=~", "!~", "!=", "="} {
			if strings.HasPrefix(s, op) {
				m.op = op
				break
			}
		}
		if m.op == "" {
			return nil, fmt.Errorf("matcher operator expected at %q", s)
		}
		s = s[len(m.op):]
		lit, err := strconv.QuotedPrefix(s)
		if err != nil {
			return nil, fmt.Errorf("quoted value expected at %q: %w", s, err)
		}
		if m.val, err = strconv.Unquote(lit); err != nil {
			return nil, err
		}
		s = s[len(lit):]
		if m.op == "=~" || m.op == "!~" {
			if m.re, err = regexp.Compile("^(?:" + m.val + ")$"); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
}
