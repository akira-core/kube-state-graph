package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// graphEndpoints is a minimal valid request for each graph endpoint.
var graphEndpoints = []struct {
	name, path string
	params     url.Values
}{
	{"graph", "/v1/graph", url.Values{"start": {"1746442800"}, "end": {"1746446400"}}},
	{"storage", "/v1/storage-graph", url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}},
	{"k8s", "/v1/k8s-graph", url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "cluster": {"c1"}, "az": {"zone-a"}, "env": {"prod"}, "node": {"worker-1"}}},
}

func withParams(base url.Values, extra url.Values) url.Values {
	out := url.Values{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// longValues returns n distinct values of exactly length bytes.
func longValues(prefix string, n, length int) []string {
	out := make([]string, n)
	for i := range out {
		v := fmt.Sprintf("%s-%04d-", prefix, i)
		out[i] = v + strings.Repeat("x", length-len(v))
	}
	return out
}

// Spec: "More than one hundred distinct values are rejected before any query".
func TestSelectorValueCap_RejectedBeforeAnyQuery(t *testing.T) {
	for _, ep := range graphEndpoints {
		for _, param := range []string{"cluster", "namespace", "az", "env"} {
			t.Run(ep.name+"/"+param, func(t *testing.T) {
				// No expectation: any upstream call fails the test.
				q := promqlmocks.NewMockQuerier(t)
				srv := httptest.NewServer(newServerWithMocks(t, q, nil).Handler())
				t.Cleanup(srv.Close)

				params := withParams(ep.params, url.Values{param: longValues(param, promql.MaxAlternationValues+1, 20)})
				resp, err := http.Get(srv.URL + ep.path + "?" + params.Encode())
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)
				var body errorBody
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
				assert.Equal(t, "invalid_scope", body.Error.Reason)
				assert.Equal(t, param+" carries more than 100 distinct values", body.Error.Message)
			})
		}
	}
}

// largestLegalStorageRequest builds the largest /v1/storage-graph request the
// parsers and the root cap admit at the default byte budget: 100 distinct
// 253-byte values for each selector parameter, and as many 253-byte bare `aggr`
// roots as fit the chunk cap. Harvest reads carry no request matcher, so the
// selectors do not shrink the root budget and the two maxima add up.
func largestLegalStorageRequest(t *testing.T) url.Values {
	t.Helper()
	params := url.Values{"start": {"1746442800"}, "end": {"1746446400"}}
	for _, p := range []string{"cluster", "namespace", "az", "env"} {
		params[p] = longValues(p, promql.MaxAlternationValues, 253)
	}
	budget := config.Defaults().NetAppQoSScopeBatchBytes
	var roots []string
	for n := 1; ; n++ {
		next := longValues("aggr", n, 253)
		if len(promql.ChunkScope(next, budget)) > 16 {
			break
		}
		roots = next
	}
	params["aggr"] = roots
	return params
}

// Spec: "The largest legal request is admitted" and "An oversized request line
// is refused before parsing". Driven through the production http.Server over a
// real listener: a ResponseRecorder never applies MaxHeaderBytes.
func TestMaxHeaderBytes_AdmitsTheLargestLegalRequest(t *testing.T) {
	base := startProductionServer(t)

	params := largestLegalStorageRequest(t)
	target := base + "/v1/storage-graph?" + params.Encode()
	t.Logf("largest legal request line: %d bytes, %d aggr roots", len(target), len(params["aggr"]))
	require.Greater(t, len(target), 128<<10, "the request is near the bound, not trivially under it")

	resp, err := http.Get(target)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "admitted and answered, not 431")

	// One more root fills a seventeenth chunk: still read, then refused by the cap.
	over := withParams(params, url.Values{"aggr": longValues("aggr", len(params["aggr"])+1, 253)})
	resp, err = http.Get(base + "/v1/storage-graph?" + over.Encode())
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a root set past the cap is a 400, not a 431")

	huge := base + "/v1/graph?start=1746442800&end=1746446400&x=" + strings.Repeat("y", MaxHeaderBytes+8192)
	resp, err = http.Get(huge)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
}

// startProductionServer serves a mock-backed handler through NewHTTPServer on a
// real listener and returns its base URL.
func startProductionServer(t *testing.T) string {
	t.Helper()
	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	httpSrv := NewHTTPServer(cfg, s.Handler())
	require.Equal(t, MaxHeaderBytes, httpSrv.MaxHeaderBytes)
	require.Equal(t, 256<<10, httpSrv.MaxHeaderBytes)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	return "http://" + ln.Addr().String()
}

// podRootRequest is a /v1/storage-graph request rooted at n pods of one
// namespace ns, each name nameLen bytes long.
func podRootRequest(ns string, n, nameLen int) url.Values {
	params := url.Values{"start": {"1746442800"}, "end": {"1746446400"}, "az": {"zone-a"}, "env": {"prod"}}
	for _, name := range longValues("p", n, nameLen) {
		params.Add("pod", ns+"/"+name)
	}
	return params
}

// A prefixed root (`pod=<ns>/<name>`, qualified `aggr=<oc>/<name>`) repeats its
// prefix in every URL value while the chunk budget charges it once per chunk,
// so its request line is the one that can outgrow the bound. With a
// Kubernetes-legal namespace (a DNS label, at most 63 bytes) the largest pod
// root set the caps admit still fits; a parser-legal 170-byte prefix does not
// and is refused with 431 — the documented limit of design D2.
func TestMaxHeaderBytes_PrefixedRoots(t *testing.T) {
	base := startProductionServer(t)
	budget := config.Defaults().NetAppQoSScopeBatchBytes
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	ns := "ns-" + strings.Repeat("n", 60) // 63 bytes, the longest namespace

	// The largest Kubernetes-legal pod root set: for every name length, the
	// most pods preparePodSeed's name split admits in the cap's 16 chunks.
	nameBudget := budget - promql.RequestMatcherCost(promql.QPVCBindings, promql.LabelKeys{AZ: "az", Env: "env"}, sel) -
		promql.MatcherCost(promql.NamespaceLabel, []string{ns}) - 1
	var worst url.Values
	worstLen := 0
	for nameLen := 12; nameLen <= 253; nameLen++ {
		// ChunkScope packs greedily, so the first 16 chunks of a set larger
		// than any cap-admitted one hold exactly the most it admits.
		n := 0
		for _, chunk := range promql.ChunkScope(longValues("p", 16*promql.MaxAlternationValues+1, nameLen), nameBudget)[:16] {
			n += len(chunk)
		}
		if req := podRootRequest(ns, n, nameLen); len(req.Encode()) > worstLen {
			worst, worstLen = req, len(req.Encode())
		}
	}
	target := base + "/v1/storage-graph?" + worst.Encode()
	t.Logf("largest Kubernetes-legal pod root request line: %d bytes, %d pods", len(target), len(worst["pod"]))
	resp, err := http.Get(target)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "admitted by the bound and by both pod caps")

	corner := podRootRequest("ns-"+strings.Repeat("n", 167), 1600, 70)
	resp, err = http.Get(base + "/v1/storage-graph?" + corner.Encode())
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode,
		"a parser-legal 170-byte namespace prefix repeated 1600 times exceeds the bound")
}

// Spec: "Many cluster values are truncated in the log".
func TestAccessLog_ClustersAreTruncated(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	h := s.Handler()

	clusters := longValues("c", 50, 12)
	clusters[3] = strings.Repeat("é", 100) // 200 bytes, cut at a rune boundary
	params := withParams(graphEndpoints[0].params, url.Values{"cluster": clusters})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/graph?"+params.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code)

	line := accessLogLine(t, &buf)
	got, ok := line["clusters"].([]any)
	require.True(t, ok, "clusters is a list: %v", line)
	require.Len(t, got, maxLogClusters)
	for i, v := range got {
		s := v.(string)
		assert.LessOrEqual(t, len(s), maxLogClusterBytes, "value %d", i)
	}
	assert.Equal(t, strings.Repeat("é", 32), got[3], "cut at a rune boundary")
	assert.Equal(t, clusters[0], got[0], "request order")
	assert.Equal(t, true, line["clusters_truncated"])

	// A request within the bounds logs every value and no truncation flag.
	buf.Reset()
	params = withParams(graphEndpoints[0].params, url.Values{"cluster": {"c1", "c2"}})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/graph?"+params.Encode(), nil))
	line = accessLogLine(t, &buf)
	assert.Equal(t, []any{"c1", "c2"}, line["clusters"])
	assert.NotContains(t, line, "clusters_truncated")
}

// accessLogLine returns the one `http` access-log record in buf.
func accessLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for raw := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &rec))
		if rec["msg"] == "http" {
			return rec
		}
	}
	t.Fatalf("no access-log line in %q", buf.String())
	return nil
}
