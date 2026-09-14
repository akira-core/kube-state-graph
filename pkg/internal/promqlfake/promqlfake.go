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
