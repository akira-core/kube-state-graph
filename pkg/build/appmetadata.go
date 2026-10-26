package build

import (
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// appNamespaceKey is one namespace inside a cluster identity.
type appNamespaceKey struct{ cluster, namespace string }

// appDeployerKey is one Application inside a namespace. The app component is
// the row's own app label, not the pod's resolved Application.
type appDeployerKey struct{ cluster, namespace, app string }

// nsGroupPick is the namespace group and the owner carried by the row that
// selected it. owner is empty when that row named none.
type nsGroupPick struct {
	name  string
	owner string
}

// appMetadataIndex is the application-metadata picks built from one
// tlast_over_time vector. Empty maps (never nil) mean the vector contributed
// nothing. Callers that did not read the metric leave the graph maps nil
// instead of storing this value. unparsedDeployTimes counts non-empty
// deploy-time values that failed to parse on rows sharing the winning
// last-seen time and user. It is zero when every such value parsed or the
// label is absent.
type appMetadataIndex struct {
	podApps             map[podNameKey]string
	nsOwners            map[appNamespaceKey]string
	nsGroups            map[appNamespaceKey]nsGroupPick
	nsAliases           map[appNamespaceKey]string
	deployers           map[appDeployerKey]string
	deployTimes         map[appDeployerKey]time.Time
	unparsedDeployTimes int
}

type appMetadataSeen struct {
	value string
	at    model.SampleValue
}

type groupCandidate struct {
	name  string
	owner string
	at    model.SampleValue
}

// deployerCandidate is the winning (last-seen, user) cohort of one
// Application. when / ok are the best deploy time of that cohort, and
// unparsed counts its rows whose non-empty deploy time failed to parse.
type deployerCandidate struct {
	user     string
	at       model.SampleValue
	when     time.Time
	ok       bool
	unparsed int
}

// resolveAppMetadata builds the indexes from one vector. Each pick skips an
// empty value before it records, so an empty row never masks a populated
// sibling. Pod Application is the lexically-smallest non-empty app per
// (cluster, namespace, pod). Namespace owner and namespace alias are the
// latest-seen value, with the lexically-smallest value breaking an exact
// timestamp tie. The namespace group and its owner are one pick: latest
// non-empty group, then the lexically-smallest group, then a non-empty owner
// before an empty one, then the lexically-smallest owner. The owner is never
// borrowed from another row. The deployer and its deploy time are one pick
// keyed by the row's own app label: latest non-empty user, then the
// lexically-smallest user, then a parsable deploy time before an unparsable
// one, then the later parsed instant. A row with an empty pod still feeds
// the namespace picks. Cluster keys go through clusterResolver.bucket. The
// sample and its Metric map are read only.
func resolveAppMetadata(vec model.Vector, src promql.AppMetadataSource, mc *clusterResolver) appMetadataIndex {
	podSeen := make(map[podNameKey]string, len(vec))
	ownerSeen := make(map[appNamespaceKey]appMetadataSeen, len(vec))
	aliasSeen := make(map[appNamespaceKey]appMetadataSeen, len(vec))
	groupSeen := make(map[appNamespaceKey]groupCandidate, len(vec))
	deployerSeen := make(map[appDeployerKey]deployerCandidate, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QAppMetadata, s.Metric)
		ns := metricLabel(s.Metric, src.Namespace)
		pod := metricLabel(s.Metric, src.Pod)
		app := metricLabel(s.Metric, src.App)
		nsKey := appNamespaceKey{cluster, ns}
		if pod != "" && app != "" {
			key := podNameKey{cluster, ns, pod}
			if cur, ok := podSeen[key]; !ok || app < cur {
				podSeen[key] = app
			}
		}
		if owner := metricLabel(s.Metric, src.NSOwner); owner != "" {
			keepLatest(ownerSeen, nsKey, owner, s.Value)
		}
		if group := metricLabel(s.Metric, src.NSGroup); group != "" {
			keepGroup(groupSeen, nsKey, group, metricLabel(s.Metric, src.NSGroupOwner), s.Value)
		}
		if alias := metricLabel(s.Metric, src.NSAlias); alias != "" {
			keepLatest(aliasSeen, nsKey, alias, s.Value)
		}
		if app != "" {
			if user := metricLabel(s.Metric, src.User); user != "" {
				keepDeployer(deployerSeen, appDeployerKey{cluster, ns, app}, user, metricLabel(s.Metric, src.DeployTime), s.Value)
			}
		}
	}
	out := appMetadataIndex{
		podApps:     podSeen,
		nsOwners:    make(map[appNamespaceKey]string, len(ownerSeen)),
		nsGroups:    make(map[appNamespaceKey]nsGroupPick, len(groupSeen)),
		nsAliases:   make(map[appNamespaceKey]string, len(aliasSeen)),
		deployers:   make(map[appDeployerKey]string, len(deployerSeen)),
		deployTimes: make(map[appDeployerKey]time.Time),
	}
	for key, seen := range ownerSeen {
		out.nsOwners[key] = seen.value
	}
	for key, seen := range groupSeen {
		out.nsGroups[key] = nsGroupPick{name: seen.name, owner: seen.owner}
	}
	for key, seen := range aliasSeen {
		out.nsAliases[key] = seen.value
	}
	for key, seen := range deployerSeen {
		out.deployers[key] = seen.user
		if seen.ok {
			out.deployTimes[key] = seen.when
		}
		out.unparsedDeployTimes += seen.unparsed
	}
	return out
}

// namespaceMetadataOf copies the namespace picks onto the graph's key. The
// returned map is non-nil, including when nothing contributed. An entry
// exists only when at least one field resolved.
func namespaceMetadataOf(idx appMetadataIndex) map[graph.NamespaceRef]graph.NamespaceMeta {
	out := make(map[graph.NamespaceRef]graph.NamespaceMeta, len(idx.nsOwners)+len(idx.nsGroups)+len(idx.nsAliases))
	for key, owner := range idx.nsOwners {
		ref := graph.NamespaceRef{Cluster: key.cluster, Namespace: key.namespace}
		meta := out[ref]
		meta.Owner = owner
		out[ref] = meta
	}
	for key, group := range idx.nsGroups {
		ref := graph.NamespaceRef{Cluster: key.cluster, Namespace: key.namespace}
		meta := out[ref]
		meta.Group = group.name
		meta.GroupOwner = group.owner
		out[ref] = meta
	}
	for key, alias := range idx.nsAliases {
		ref := graph.NamespaceRef{Cluster: key.cluster, Namespace: key.namespace}
		meta := out[ref]
		meta.Alias = alias
		out[ref] = meta
	}
	return out
}

// applicationMetadataOf copies the deployer pick onto the graph's key. The
// returned map is non-nil, including when nothing contributed. An entry
// exists only when the deployer resolved. DeployedAt is zero when no deploy
// time on the winning rows parsed.
func applicationMetadataOf(idx appMetadataIndex) map[graph.ApplicationRef]graph.ApplicationMeta {
	out := make(map[graph.ApplicationRef]graph.ApplicationMeta, len(idx.deployers))
	for key, user := range idx.deployers {
		out[graph.ApplicationRef{Cluster: key.cluster, Namespace: key.namespace, Application: key.app}] = graph.ApplicationMeta{
			DeployedBy: user,
			DeployedAt: idx.deployTimes[key],
		}
	}
	return out
}

// keepGroup records the group when it is newer, or equal in time and a
// smaller name, or the same group with a better owner. An empty group is the
// caller's concern. The owner travels with the group and is never taken from
// a row that did not win.
func keepGroup(dst map[appNamespaceKey]groupCandidate, key appNamespaceKey, name, owner string, at model.SampleValue) {
	cur, ok := dst[key]
	if !ok {
		dst[key] = groupCandidate{name: name, owner: owner, at: at}
		return
	}
	if at < cur.at || (at == cur.at && name > cur.name) {
		return
	}
	if at == cur.at && name == cur.name && !betterOwner(owner, cur.owner) {
		return
	}
	dst[key] = groupCandidate{name: name, owner: owner, at: at}
}

// betterOwner reports whether next should replace cur on an exact group tie.
// A non-empty owner beats an empty one. Otherwise the lexically-smallest
// owner wins. An equal owner keeps the current row.
func betterOwner(next, cur string) bool {
	if (next != "") != (cur != "") {
		return next != ""
	}
	return next < cur
}

// keepDeployer records the user when it is newer, or equal in time and a
// smaller name. A row of the current (time, user) cohort refines its deploy
// time instead: a parsable time beats an unparsable one, and a later parsed
// instant beats an earlier one. A non-empty raw value that fails to parse is
// counted against its cohort, so only the winning cohort's failures survive.
// An empty user is the caller's concern.
func keepDeployer(dst map[appDeployerKey]deployerCandidate, key appDeployerKey, user, raw string, at model.SampleValue) {
	var when time.Time
	var parsed bool
	if raw != "" {
		when, parsed = parseDeployTime(raw)
	}
	failed := 0
	if raw != "" && !parsed {
		failed = 1
	}
	cur, exists := dst[key]
	switch {
	case !exists || at > cur.at || (at == cur.at && user < cur.user):
		dst[key] = deployerCandidate{user: user, at: at, when: when, ok: parsed, unparsed: failed}
	case at == cur.at && user == cur.user:
		if parsed && (!cur.ok || when.After(cur.when)) {
			cur.when, cur.ok = when, true
		}
		cur.unparsed += failed
		dst[key] = cur
	}
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
