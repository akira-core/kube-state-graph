package build

import (
	"math"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func defaultAppMetadata() promql.AppMetadataSource {
	return promql.AppMetadataSource{
		Metric:       "argocd_app_pod_info",
		App:          "argocd_app",
		User:         "user",
		NSOwner:      "owner",
		Namespace:    "namespace",
		Pod:          "pod",
		DeployTime:   promql.DefaultAppMetadataDeployTimeLabel,
		NSGroup:      promql.DefaultAppMetadataNSGroupLabel,
		NSGroupOwner: promql.DefaultAppMetadataNSGroupOwnerLabel,
		NSAlias:      promql.DefaultAppMetadataNSAliasLabel,
	}
}

func appMetaSample(labels map[string]string, at model.SampleValue) *model.Sample {
	m := model.Metric{}
	for k, v := range labels {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return &model.Sample{Metric: m, Value: at}
}

func appMetaBase(pod, app, owner, user string) map[string]string {
	return map[string]string{
		"az":         "us",
		"env":        "dev",
		"cluster":    "c1",
		"namespace":  "shop",
		"pod":        pod,
		"argocd_app": app,
		"owner":      owner,
		"user":       user,
	}
}

func TestResolveAppMetadata_SmallestNonEmptyAppWins(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "", "team-a", "alice"), 1),
		appMetaSample(appMetaBase("web-1", "shop-b", "team-a", "alice"), 2),
		appMetaSample(appMetaBase("web-1", "shop-a", "team-a", "alice"), 3),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "shop-a", got.podApps[podNameKey{"us-dev-c1", "shop", "web-1"}])
}

func TestResolveAppMetadata_AppIsNotSplitOnColon(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "shop:v2", "team-a", "alice"), 1),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "shop:v2", got.podApps[podNameKey{"us-dev-c1", "shop", "web-1"}])
}

func TestResolveAppMetadata_LatestOwnerWins(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "checkout", "team-a", "alice"), 1),
		appMetaSample(appMetaBase("web-2", "checkout", "team-b", "bob"), 2),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "team-b", got.nsOwners[appNamespaceKey{"us-dev-c1", "shop"}])
}

func TestResolveAppMetadata_OwnerTieBrokenLexically(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "checkout", "team-b", "alice"), 5),
		appMetaSample(appMetaBase("web-2", "checkout", "team-a", "bob"), 5),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "team-a", got.nsOwners[appNamespaceKey{"us-dev-c1", "shop"}])
}

func TestResolveAppMetadata_DeployerKeyedByTheRowsOwnApp(t *testing.T) {
	// The pod's Application is the lexically-smallest app (shop-a). The
	// deployer of shop-b stays alice: it is keyed by the row's own app, not
	// by the pod's resolved Application.
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "shop-b", "team-a", "alice"), 1),
		appMetaSample(appMetaBase("web-1", "shop-a", "team-a", "bob"), 2),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "shop-a", got.podApps[podNameKey{"us-dev-c1", "shop", "web-1"}])
	assert.Equal(t, "alice", got.deployers[appDeployerKey{"us-dev-c1", "shop", "shop-b"}])
	assert.Equal(t, "bob", got.deployers[appDeployerKey{"us-dev-c1", "shop", "shop-a"}])
}

func TestResolveAppMetadata_LatestDeployerWins(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "checkout", "team-a", "alice"), 1),
		appMetaSample(appMetaBase("web-2", "checkout", "team-a", "bob"), 2),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "bob", got.deployers[appDeployerKey{"us-dev-c1", "shop", "checkout"}])
}

func TestResolveAppMetadata_EmptyPodStillFeedsOwnerAndDeployer(t *testing.T) {
	labels := appMetaBase("", "checkout", "team-z", "carol")
	vec := model.Vector{appMetaSample(labels, 9)}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Empty(t, got.podApps)
	assert.Equal(t, "team-z", got.nsOwners[appNamespaceKey{"us-dev-c1", "shop"}])
	assert.Equal(t, "carol", got.deployers[appDeployerKey{"us-dev-c1", "shop", "checkout"}])
}

func TestResolveAppMetadata_EmptyValuesNeverMask(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "shop-a", "team-a", "alice"), 1),
		appMetaSample(appMetaBase("web-1", "", "", ""), 9),
	}
	got := resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	assert.Equal(t, "shop-a", got.podApps[podNameKey{"us-dev-c1", "shop", "web-1"}])
	assert.Equal(t, "team-a", got.nsOwners[appNamespaceKey{"us-dev-c1", "shop"}])
	assert.Equal(t, "alice", got.deployers[appDeployerKey{"us-dev-c1", "shop", "shop-a"}])
}

func TestResolveAppMetadata_ReadsConfiguredLabelsAndZoneKeys(t *testing.T) {
	src := defaultAppMetadata()
	src.App = "application"
	src.User = "deployer"
	src.NSOwner = "ns_owner"
	src.Namespace = "k8s_namespace"
	src.Pod = "workload"
	labels := map[string]string{
		"zone":          "us",
		"env":           "dev",
		"cluster":       "c1",
		"namespace":     "ignored-ns",
		"pod":           "ignored-pod",
		"argocd_app":    "ignored-app",
		"owner":         "ignored-owner",
		"user":          "ignored-user",
		"k8s_namespace": "shop",
		"workload":      "web-1",
		"application":   "checkout",
		"ns_owner":      "team-a",
		"deployer":      "alice",
	}
	keys := promql.LabelKeys{AZ: "zone", Env: "env"}
	got := resolveAppMetadata(model.Vector{appMetaSample(labels, 1)}, src, newClusterResolver(keys))
	assert.Equal(t, "checkout", got.podApps[podNameKey{"us-dev-c1", "shop", "web-1"}])
	assert.Equal(t, "team-a", got.nsOwners[appNamespaceKey{"us-dev-c1", "shop"}])
	assert.Equal(t, "alice", got.deployers[appDeployerKey{"us-dev-c1", "shop", "checkout"}])
	assert.NotContains(t, got.podApps, podNameKey{"c1", "ignored-ns", "ignored-pod"})
}

func TestResolveAppMetadata_DoesNotMutateSamples(t *testing.T) {
	vec := model.Vector{
		appMetaSample(appMetaBase("web-1", "shop-b", "team-b", "bob"), 2),
		appMetaSample(appMetaBase("web-1", "shop-a", "team-a", "alice"), 2),
	}
	before := make([]struct {
		fp    model.Fingerprint
		value uint64
	}, len(vec))
	for i, s := range vec {
		before[i].fp = s.Metric.Fingerprint()
		before[i].value = math.Float64bits(float64(s.Value))
	}
	_ = resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	for i, s := range vec {
		assert.Equal(t, before[i].fp, s.Metric.Fingerprint())
		assert.Equal(t, before[i].value, math.Float64bits(float64(s.Value)))
	}
}

func TestParseTopology_MetricApplicationWinsOverAnnotation(t *testing.T) {
	src := defaultAppMetadata()
	tp := parseTopology(topologyVectors{
		Pod:      sampleVec(appPod("shop", "checkout-1", "u1"), appPod("shop", "checkout-2", "u2")),
		PodOwner: sampleVec(appOwner("shop", "checkout-1", "ReplicaSet", "checkout-7f9c"), appOwner("shop", "checkout-2", "ReplicaSet", "checkout-7f9c")),
		ReplicaSetOwner: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "shop", "replicaset": "checkout-7f9c",
			"owner_kind": "Deployment", "owner_name": "checkout",
		}}),
		DeploymentAnnotations: sampleVec(ctrlAnn("deployment", "shop", "checkout", "storefront:apps/Deployment:shop/checkout")),
		AppMetadata: sampleVec(
			model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "shop", "pod": "checkout-1", "argocd_app": "shop-v2",
			}, Value: 1},
		),
		AppMetadataSource: &src,
	}, promql.LabelKeys{})

	byName := map[string]*graph.PodNode{}
	for _, p := range tp.Pods {
		byName[p.Name()] = p
	}
	assert.Equal(t, "shop-v2", byName["checkout-1"].Application())
	require.NotNil(t, byName["checkout-1"].Owner())
	assert.Equal(t, graph.Owner{Kind: "Deployment", Name: "checkout"}, *byName["checkout-1"].Owner())
	assert.Equal(t, "storefront", byName["checkout-2"].Application(), "a pod with no metric row keeps the controller chain")
}

func TestParseTopology_MetricResolvesRolloutPod(t *testing.T) {
	src := defaultAppMetadata()
	tp := parseTopology(topologyVectors{
		Pod:      sampleVec(appPod("shop", "canary-1", "u1")),
		PodOwner: sampleVec(appOwner("shop", "canary-1", "Rollout", "canary")),
		AppMetadata: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "shop", "pod": "canary-1", "argocd_app": "canary-app",
		}, Value: 1}),
		AppMetadataSource: &src,
	}, promql.LabelKeys{})

	require.Len(t, tp.Pods, 1)
	assert.Equal(t, "canary-app", tp.Pods[0].Application())
	require.NotNil(t, tp.Pods[0].Owner())
	assert.Equal(t, graph.Owner{Kind: "Rollout", Name: "canary"}, *tp.Pods[0].Owner())
}

func TestParseTopology_PVCInheritsMetricApplication(t *testing.T) {
	src := defaultAppMetadata()
	tp := parseTopology(topologyVectors{
		Pod:                   sampleVec(appPod("shop", "checkout-1", "u1")),
		PodOwner:              sampleVec(appOwner("shop", "checkout-1", "Deployment", "checkout")),
		DeploymentAnnotations: sampleVec(ctrlAnn("deployment", "shop", "checkout", "storefront:apps/Deployment:shop/checkout")),
		PVC: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "shop", "pod": "checkout-1", "persistentvolumeclaim": "data-1",
		}}),
		AppMetadata: sampleVec(model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "shop", "pod": "checkout-1", "argocd_app": "shop-v2",
		}, Value: 1}),
		AppMetadataSource: &src,
	}, promql.LabelKeys{})

	require.Len(t, tp.PVCs, 1)
	assert.Equal(t, "shop-v2", tp.PVCs[0].Application())
	_, hasApp := tp.PVCs[0].Labels()["application"]
	assert.False(t, hasApp)
}

func TestResolveAppMetadata_OrderIndependent(t *testing.T) {
	a := appMetaSample(appMetaBase("web-1", "shop-b", "team-b", "zoe"), 4)
	b := appMetaSample(appMetaBase("web-1", "shop-a", "team-a", "amy"), 4)
	forward := resolveAppMetadata(model.Vector{a, b}, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	reverse := resolveAppMetadata(model.Vector{b, a}, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
	require.Equal(t, forward, reverse)
}
