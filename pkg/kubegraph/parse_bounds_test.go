package kubegraph_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// parsers names the three request parsers with a minimal valid request for each,
// so a bound is asserted on every endpoint at once.
var parsers = []struct {
	name  string
	base  func() url.Values
	parse func(url.Values) error
}{
	{"graph", func() url.Values {
		return url.Values{"start": {"1700000000"}, "end": {"1700003600"}}
	}, func(v url.Values) error { _, err := kubegraph.ParseValues(v); return err }},
	{"storage", func() url.Values {
		return url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}
	}, func(v url.Values) error { _, err := kubegraph.ParseStorageValues(v); return err }},
	{"k8s", func() url.Values {
		return with(k8sBase(), map[string][]string{"node": {"worker-1"}})
	}, func(v url.Values) error { _, err := kubegraph.ParseK8sValues(v); return err }},
}

// distinctValues returns n distinct values of param, e.g. namespace-000.
func distinctValues(param string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%03d", param, i)
	}
	return out
}

// Spec: "One hundred distinct values are accepted" and "More than one hundred
// distinct values are rejected before any query".
func TestParse_SelectorValueCountIsBounded(t *testing.T) {
	require.Equal(t, 100, promql.MaxAlternationValues)
	for _, p := range parsers {
		for _, param := range []string{"cluster", "namespace", "az", "env"} {
			t.Run(p.name+"/"+param, func(t *testing.T) {
				atCap := with(p.base(), map[string][]string{param: distinctValues(param, promql.MaxAlternationValues)})
				require.NoError(t, p.parse(atCap), "100 distinct values pass")

				// 101 values of which 100 are distinct, plus empties, still pass.
				dup := append(distinctValues(param, promql.MaxAlternationValues), param+"-000", "", "")
				require.NoError(t, p.parse(with(p.base(), map[string][]string{param: dup})),
					"a repeated or empty value does not count")

				over := distinctValues(param, promql.MaxAlternationValues+1)
				err := p.parse(with(p.base(), map[string][]string{param: over}))
				pe, ok := errAsParse(err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, "invalid_scope", pe.Reason)
				assert.Equal(t, param+" carries more than 100 distinct values", pe.Message)
				assert.NotContains(t, pe.Message, param+"-0", "no value is echoed")
			})
		}
	}
}

// Spec: "Required parameters are checked first".
func TestParse_RequiredParametersWinOverTheCount(t *testing.T) {
	over := distinctValues("namespace", promql.MaxAlternationValues+1)

	_, err := kubegraph.ParseStorageValues(url.Values{
		"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod"}, "aggr": {"a"}, "namespace": over,
	})
	pe, ok := errAsParse(err)
	require.True(t, ok)
	assert.Equal(t, "missing_az", pe.Reason)

	_, err = kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"cluster": nil, "namespace": over, "node": {"w"}}))
	pe, ok = errAsParse(err)
	require.True(t, ok)
	assert.Equal(t, "missing_cluster", pe.Reason)

	// A malformed value of the same parameter is still reported as such.
	bad := append(distinctValues("cluster", promql.MaxAlternationValues+1), "bad\nvalue")
	_, err = kubegraph.ParseValues(url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "cluster": bad})
	pe, ok = errAsParse(err)
	require.True(t, ok)
	assert.Equal(t, "cluster value contains a control character", pe.Message)
}

// Root parameters are bounded by the build's chunk caps, not by the parser's
// value count: a legal root set holds far more than 100 values.
func TestParse_RootParametersAreNotCountCapped(t *testing.T) {
	v := url.Values{
		"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"},
		"aggr": distinctValues("aggr", 1000),
	}
	_, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
}

// Spec: "A short malformed timestamp is echoed" and "An oversized malformed
// value is not echoed".
func TestParse_RejectedRawValuesAreEchoedOnlyWhenBounded(t *testing.T) {
	base := url.Values{"start": {"1700000000"}, "end": {"1700003600"}}

	_, err := kubegraph.ParseValues(with(base, map[string][]string{"start": {"yesterday"}}))
	pe, ok := errAsParse(err)
	require.True(t, ok)
	assert.Equal(t, "invalid_start", pe.Reason)
	assert.Equal(t, `timestamp must be RFC 3339 or Unix seconds: "yesterday"`, pe.Message, "byte-identical for a short value")

	_, err = kubegraph.ParseValues(with(base, map[string][]string{"prune": {"maybe"}}))
	pe, ok = errAsParse(err)
	require.True(t, ok)
	assert.Equal(t, `prune must be true or false, got "maybe"`, pe.Message, "byte-identical for a short value")

	long := strings.Repeat("z", 300)
	for _, tc := range []struct {
		param, reason, want string
	}{
		{"end", "invalid_end", "timestamp must be RFC 3339 or Unix seconds"},
		{"start", "invalid_start", "timestamp must be RFC 3339 or Unix seconds"},
		{"prune", "invalid_scope", "prune must be true or false"},
	} {
		for _, raw := range []string{long, "bad\x01value", "bad\xffvalue"} {
			_, err := kubegraph.ParseValues(with(base, map[string][]string{tc.param: {raw}}))
			pe, ok := errAsParse(err)
			require.True(t, ok, "%s=%q", tc.param, raw)
			assert.Equal(t, tc.reason, pe.Reason)
			assert.Equal(t, tc.want, pe.Message, "%s: the value is not echoed", tc.param)
		}
	}

	// The storage and k8s parsers share the window and the message.
	_, err = kubegraph.ParseStorageValues(url.Values{"start": {long}, "end": {"1700003600"}, "az": {"a"}, "env": {"e"}, "aggr": {"x"}})
	pe, ok = errAsParse(err)
	require.True(t, ok)
	assert.NotContains(t, pe.Message, long)
}

func errAsParse(err error) (*kubegraph.ParseError, bool) {
	pe, ok := err.(*kubegraph.ParseError)
	return pe, ok
}
