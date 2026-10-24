package build

import (
	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// appNamespaceKey is one namespace inside a cluster identity.
type appNamespaceKey struct{ cluster, namespace string }

// appDeployerKey is one Application inside a namespace. The app component is
// the row's own app label, not the pod's resolved Application.
type appDeployerKey struct{ cluster, namespace, app string }

// appMetadataIndex is the three application-metadata picks built from one
// tlast_over_time vector. Empty maps (never nil) mean the vector contributed
// nothing. Callers that did not read the metric leave the graph maps nil
// instead of storing this value.
type appMetadataIndex struct {
	podApps   map[podNameKey]string
	nsOwners  map[appNamespaceKey]string
	deployers map[appDeployerKey]string
}

type appMetadataSeen struct {
	value string
	at    model.SampleValue
}

// resolveAppMetadata builds the three indexes from one vector. Each index
// skips an empty value before the pick, so an empty row never masks a
// populated sibling. Pod Application is the lexically-smallest non-empty app
// per (cluster, namespace, pod). Namespace owner and Application deployer are
// the latest-seen value, with the lexically-smallest value breaking an exact
// timestamp tie. The deployer is keyed by the row's own app label. A row with
// an empty pod still feeds the owner and deployer indexes. Cluster keys go
// through clusterResolver.bucket. The sample and its Metric map are read only.
func resolveAppMetadata(vec model.Vector, src promql.AppMetadataSource, mc *clusterResolver) appMetadataIndex {
	podSeen := make(map[podNameKey]string, len(vec))
	ownerSeen := make(map[appNamespaceKey]appMetadataSeen, len(vec))
	deployerSeen := make(map[appDeployerKey]appMetadataSeen, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QAppMetadata, s.Metric)
		ns := metricLabel(s.Metric, src.Namespace)
		pod := metricLabel(s.Metric, src.Pod)
		app := metricLabel(s.Metric, src.App)
		if pod != "" && app != "" {
			key := podNameKey{cluster, ns, pod}
			if cur, ok := podSeen[key]; !ok || app < cur {
				podSeen[key] = app
			}
		}
		if owner := metricLabel(s.Metric, src.NSOwner); owner != "" {
			key := appNamespaceKey{cluster, ns}
			keepLatest(ownerSeen, key, owner, s.Value)
		}
		if app != "" {
			if user := metricLabel(s.Metric, src.User); user != "" {
				key := appDeployerKey{cluster, ns, app}
				keepLatest(deployerSeen, key, user, s.Value)
			}
		}
	}
	out := appMetadataIndex{
		podApps:   podSeen,
		nsOwners:  make(map[appNamespaceKey]string, len(ownerSeen)),
		deployers: make(map[appDeployerKey]string, len(deployerSeen)),
	}
	for key, seen := range ownerSeen {
		out.nsOwners[key] = seen.value
	}
	for key, seen := range deployerSeen {
		out.deployers[key] = seen.value
	}
	return out
}

// namespaceOwnersOf copies the namespace-owner index onto the graph's key.
// The returned map is non-nil, including when nothing contributed.
func namespaceOwnersOf(idx appMetadataIndex) map[graph.NamespaceRef]string {
	out := make(map[graph.NamespaceRef]string, len(idx.nsOwners))
	for key, owner := range idx.nsOwners {
		out[graph.NamespaceRef{Cluster: key.cluster, Namespace: key.namespace}] = owner
	}
	return out
}

// applicationDeployersOf copies the deployer index onto the graph's key.
// The returned map is non-nil, including when nothing contributed.
func applicationDeployersOf(idx appMetadataIndex) map[graph.ApplicationRef]string {
	out := make(map[graph.ApplicationRef]string, len(idx.deployers))
	for key, user := range idx.deployers {
		out[graph.ApplicationRef{Cluster: key.cluster, Namespace: key.namespace, Application: key.app}] = user
	}
	return out
}

func metricLabel(m model.Metric, name string) string {
	if name == "" {
		return ""
	}
	return string(m[model.LabelName(name)])
}

// keepLatest records value when it is newer than the current pick, or equal
// in time and lexically smaller. An empty value is the caller's concern.
func keepLatest[K comparable](dst map[K]appMetadataSeen, key K, value string, at model.SampleValue) {
	cur, ok := dst[key]
	if ok && (at < cur.at || (at == cur.at && value >= cur.value)) {
		return
	}
	dst[key] = appMetadataSeen{value: value, at: at}
}
