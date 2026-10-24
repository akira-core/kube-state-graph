package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// appMetadataSource is the default binding the component tests configure.
func appMetadataSource() *promql.AppMetadataSource {
	return &promql.AppMetadataSource{
		Metric:    "argocd_app_pod_info",
		App:       "argocd_app",
		User:      "user",
		NSOwner:   "owner",
		Namespace: "namespace",
		Pod:       "pod",
	}
}

// controllerChainFixtures is the happy estate plus the ReplicaSet → Deployment
// tracking-id chain that resolves web-1 to Application storefront.
func controllerChainFixtures() fixtureSet {
	fixtures := happyFixtures()
	fixtures["last_over_time(kube_pod_owner"] = vec(map[string]string{
		"cluster": "test", "namespace": "default", "pod": "web-1",
		"owner_kind": "ReplicaSet", "owner_name": "web-7f9c", "owner_is_controller": "true",
	})
	fixtures["last_over_time(kube_replicaset_owner"] = vec(map[string]string{
		"cluster": "test", "namespace": "default", "replicaset": "web-7f9c",
		"owner_kind": "Deployment", "owner_name": "web",
	})
	fixtures["last_over_time(kube_deployment_annotations"] = vec(map[string]string{
		"cluster": "test", "namespace": "default", "deployment": "web",
		"annotation_argocd_argoproj_io_tracking_id": "storefront:apps/Deployment:default/web",
	})
	return fixtures
}

// newServerWithAppMetadata is newServerWithMocks with the metric binding set.
// The shared helper stays nil so every existing component test keeps the
// metric-unset fan-out.
func newServerWithAppMetadata(t *testing.T, q *promqlmocks.MockQuerier, src *promql.AppMetadataSource) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.PromURL = "http://unused"
	require.NoError(t, cfg.Validate())
	logger := observability.NewLogger("error")
	metrics := observability.NewMetrics()
	builder := build.New(q, build.Options{
		APITimeout:  cfg.APITimeout,
		LabelKeys:   promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
		AppMetadata: src,
	}, metrics, clock.System{})
	return New(cfg, builder, q, metrics, logger, auth.NewKeySet(), clock.System{})
}

type recordedInstant struct {
	name  string
	query string
}

func recordedInstants(q *promqlmocks.MockQuerier) []recordedInstant {
	var out []recordedInstant
	for _, c := range q.Calls {
		if c.Method != "Instant" {
			continue
		}
		out = append(out, recordedInstant{name: c.Arguments.String(1), query: c.Arguments.String(2)})
	}
	return out
}

func countNamed(calls []recordedInstant, name string) int {
	n := 0
	for _, c := range calls {
		if c.name == name {
			n++
		}
	}
	return n
}

func graphBody(t *testing.T, srv *httptest.Server) (int, []byte) {
	t.Helper()
	end := time.Now().UTC()
	start := end.Add(-15 * time.Minute)
	resp, err := http.Get(graphURL(srv.URL+"/v1/graph", start, end))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func podNode(t *testing.T, raw []byte) cytoscape.NodeData {
	t.Helper()
	var body cytoscape.Body
	require.NoError(t, json.Unmarshal(raw, &body))
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == "pod" {
			return n.Data
		}
	}
	t.Fatal("expected a pod node")
	return cytoscape.NodeData{}
}

func TestGraphEndpoint_AppMetadataLeg(t *testing.T) {
	fixtures := controllerChainFixtures()
	fixtures["tlast_over_time(argocd_app_pod_info"] = vec(map[string]string{
		"cluster": "test", "namespace": "default", "pod": "web-1",
		"argocd_app": "shop-v2", "owner": "payments", "user": "alice",
	})

	unsetQ := newMockQuerier(t, fixtures)
	unset := httptest.NewServer(newServerWithMocks(t, unsetQ, nil).Handler())
	t.Cleanup(unset.Close)
	unsetStatus, unsetRaw := graphBody(t, unset)
	require.Equal(t, http.StatusOK, unsetStatus)
	unsetPod := podNode(t, unsetRaw)
	assert.Equal(t, "storefront", unsetPod.Application)
	unsetCalls := recordedInstants(unsetQ)
	assert.Zero(t, countNamed(unsetCalls, string(promql.QAppMetadata)), "an unset metric issues no application-metadata query")

	cfgQ := newMockQuerier(t, fixtures)
	cfgSrv := httptest.NewServer(newServerWithAppMetadata(t, cfgQ, appMetadataSource()).Handler())
	t.Cleanup(cfgSrv.Close)
	cfgStatus, cfgRaw := graphBody(t, cfgSrv)
	require.Equal(t, http.StatusOK, cfgStatus)
	cfgCalls := recordedInstants(cfgQ)
	assert.Len(t, cfgCalls, len(unsetCalls)+1, "the configured metric adds exactly one upstream query")
	var meta []recordedInstant
	for _, c := range cfgCalls {
		if c.name == string(promql.QAppMetadata) {
			meta = append(meta, c)
		}
		assert.NotEqual(t, "argocd_app_pod_info", c.name, "self-metrics and the tally key stay the logical name")
	}
	require.Len(t, meta, 1)
	assert.Contains(t, meta[0].query, "tlast_over_time(argocd_app_pod_info")
	assert.NotContains(t, meta[0].query, "tlast_over_time(app_metadata")

	cfgPod := podNode(t, cfgRaw)
	assert.Equal(t, "shop-v2", cfgPod.Application, "the metric wins over the controller annotation")
	require.NotNil(t, cfgPod.Owner)
	assert.Equal(t, graph.Owner{Kind: "Deployment", Name: "web"}, *cfgPod.Owner)

	errQ := newAppMetadataErrQuerier(t, fixtures)
	errSrv := httptest.NewServer(newServerWithAppMetadata(t, errQ, appMetadataSource()).Handler())
	t.Cleanup(errSrv.Close)
	errStatus, errRaw := graphBody(t, errSrv)
	require.Equal(t, http.StatusOK, errStatus)
	errPod := podNode(t, errRaw)
	assert.Equal(t, "storefront", errPod.Application, "a query error falls back to the controller chain")
	require.NotNil(t, errPod.Owner)
	assert.Equal(t, graph.Owner{Kind: "Deployment", Name: "web"}, *errPod.Owner)
	assert.NotContains(t, string(errRaw), "namespace_owner")
	assert.NotContains(t, string(errRaw), "deployed_by")
	assert.Equal(t, string(unsetRaw), string(errRaw), "a degraded build equals the metric-unset body")
	assert.Equal(t, 1, countNamed(recordedInstants(errQ), string(promql.QAppMetadata)))
}

// newAppMetadataErrQuerier is newMockQuerier, except the logical app_metadata
// call fails. The failure is keyed on the Instant name: a fixture needle would
// match the rendered metric, which is not the query the degrade contract names.
func newAppMetadataErrQuerier(t *testing.T, fixtures fixtureSet) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
			if name == string(promql.QAppMetadata) {
				return nil, errors.New("app metadata store down")
			}
			var hits []string
			var match model.Vector
			for needle, vec := range fixtures {
				if strings.Contains(query, needle) {
					hits = append(hits, needle)
					match = vec
				}
			}
			if len(hits) > 1 {
				sort.Strings(hits)
				t.Errorf("ambiguous fixture dispatch for query %q: matched needles %v", query, hits)
			}
			if len(hits) >= 1 {
				return match, nil
			}
			return model.Vector{}, nil
		}).
		Maybe()
	return q
}
