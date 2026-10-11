package build

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/prometheus/common/model"
)

// ownerRef is a resolved controller owner (kind + name) for a pod.
type ownerRef struct{ kind, name string }

// resolvePodOwners builds the (cluster, namespace, pod) → controller-owner index
// from kube_pod_owner, skipping the intermediate ReplicaSet (D34): when a pod's
// controller owner is a ReplicaSet, it is resolved one level up via
// kube_replicaset_owner to the owning Deployment. A bare ReplicaSet with no
// Deployment owner keeps the ReplicaSet as the owner; any other owner kind is
// surfaced verbatim. Pods with no controller owner are simply absent from the
// returned map (the caller omits the labels rather than emitting empty strings).
//
// The returned map is a deterministic function of the two input vectors — no
// ordering dependence: when a pod reports multiple controller owners, the
// lexically-smallest (kind, name) wins so the emitted entity is stable across
// rebuilds (D6 determinism). The only side effect is tallying missing-cluster
// samples into the caller's mc accumulator.
func resolvePodOwners(ownerVec, rsOwnerVec model.Vector, mc *clusterResolver) map[podNameKey]ownerRef {
	// ReplicaSet → owning Deployment, keyed by (cluster, namespace, replicaset).
	// Only Deployment owners are retained; a ReplicaSet owned by anything else
	// (or nothing) is left unresolved so the pod keeps the ReplicaSet.
	rsToDeployment := make(map[podNameKey]string, len(rsOwnerVec))
	for _, s := range rsOwnerVec {
		if string(s.Metric["owner_kind"]) != "Deployment" {
			continue
		}
		cluster := mc.bucket(promql.QReplicaSetOwner, s.Metric)
		ns := string(s.Metric["namespace"])
		rs := string(s.Metric["replicaset"])
		dep := string(s.Metric["owner_name"])
		if rs == "" || dep == "" {
			continue
		}
		rsToDeployment[podNameKey{cluster, ns, rs}] = dep
	}

	owners := make(map[podNameKey]ownerRef, len(ownerVec))
	for _, s := range ownerVec {
		if string(s.Metric["owner_is_controller"]) != "true" {
			continue
		}
		cluster := mc.bucket(promql.QPodOwner, s.Metric)
		ns := string(s.Metric["namespace"])
		pod := string(s.Metric["pod"])
		kind := string(s.Metric["owner_kind"])
		name := string(s.Metric["owner_name"])
		if pod == "" || kind == "" || name == "" {
			continue
		}
		if kind == "ReplicaSet" {
			if dep, ok := rsToDeployment[podNameKey{cluster, ns, name}]; ok {
				kind, name = "Deployment", dep
			}
		}
		key := podNameKey{cluster, ns, pod}
		// Deterministic pick: lexically-smallest (kind, name) wins on collision.
		if cur, ok := owners[key]; ok && !ownerLess(kind, name, cur.kind, cur.name) {
			continue
		}
		owners[key] = ownerRef{kind, name}
	}
	return owners
}

// ownerLess is the controller-owner tie-break shared by resolvePodOwners and
// ResolvePodApplication: lexically-smallest (kind, name), compared AFTER the
// ReplicaSet → Deployment collapse.
func ownerLess(kind, name, curKind, curName string) bool {
	return kind < curKind || (kind == curKind && name < curName)
}

// resolvePodContainers builds the (cluster, namespace, pod) → sorted container
// list index from kube_pod_container_info. Each series contributes one
// {name=container, image=image} element, deduped per (pod, container-name).
//
// The query is `tlast_over_time(kube_pod_container_info[w])`, so each series'
// VALUE (`s.Value`) is its last-sample timestamp (unix seconds). When a container
// changed image in the window — each image being a DISTINCT series (image is a
// label) — the image SEEN LATEST wins (the current one). Exact-timestamp ties
// (co-scraped images) break by lexically-smallest image so the body stays
// byte-identical across rebuilds (D6). Empty images are skipped so a transient
// image-less series never masks (or, by a later timestamp, beats) a populated
// sibling. The per-pod list is sorted by (name, image).
//
// CAVEAT (documented in design.md D-A4): for query windows far from the real wall
// clock, VictoriaMetrics returns only ONE image-variant series per container
// (dropping the rest) — true for last_over_time, tlast_over_time, AND
// query_range alike. So "latest" is only meaningful for near-now windows (the
// dominant case); for far-past windows the resolver simply surfaces whatever
// single variant VM returns. The pick is never worse than a lexically-smallest
// fallback would be, and degrades gracefully if the query is ever reverted to
// last_over_time (all values equal → the lexical tie-break decides).
//
// OPTIONAL: an absent or empty vector yields an empty map and pods carry no
// containers (graceful degradation). The returned map is a deterministic
// function of the input vector. The only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolvePodContainers(vec model.Vector, mc *clusterResolver) map[podNameKey][]graph.Container {
	type containerKey struct {
		pod  podNameKey
		name string
	}
	type pick struct {
		image    string
		lastSeen model.SampleValue
	}
	// (pod, container-name) → the image last seen latest (greatest tlast_over_time
	// value), lexically-smallest image breaking exact-timestamp ties.
	best := make(map[containerKey]pick, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QPodContainerInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		pod := string(s.Metric["pod"])
		name := string(s.Metric["container"])
		image := string(s.Metric["image"])
		if pod == "" || name == "" || image == "" {
			continue
		}
		key := containerKey{podNameKey{cluster, ns, pod}, name}
		if cur, ok := best[key]; ok {
			if s.Value < cur.lastSeen || (s.Value == cur.lastSeen && image >= cur.image) {
				continue
			}
		}
		best[key] = pick{image: image, lastSeen: s.Value}
	}

	out := map[podNameKey][]graph.Container{}
	for key, p := range best {
		out[key.pod] = append(out[key.pod], graph.Container{Name: key.name, Image: p.image})
	}
	for pod := range out {
		list := out[pod]
		slices.SortStableFunc(list, func(a, b graph.Container) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Image, b.Image))
		})
	}
	return out
}

// controllerKey identifies one workload controller by its cluster-scoped
// namespace, its owner kind and its name — exactly the tuple resolvePodOwners
// already produces per pod, so the pod → Application join is a single lookup.
type controllerKey struct{ cluster, namespace, kind, name string }

// controllerAnnotationFamily binds one owner kind to the kube-state-metrics
// annotation family that describes it and to that family's resource-identity
// label. The Job family's identity label is `job_name`, NOT `job` —
// kube-state-metrics avoids Prometheus' reserved `job` target label.
type controllerAnnotationFamily struct {
	kind      string
	query     promql.Query
	nameLabel model.LabelName
	vec       func(topologyVectors) model.Vector
}

// controllerAnnotationFamilies is the complete set of pod controller kinds a
// stock kube-state-metrics can describe. A resolved owner kind absent from this
// table — ReplicationController (KSM exposes no annotations family for it),
// Node (static / mirror pods), or any CRD controller such as argo-rollouts
// Rollout or OpenKruise CloneSet — resolves no Application, keeps the pod's
// owner attribute, and never fails the build.
var controllerAnnotationFamilies = []controllerAnnotationFamily{
	{"Deployment", promql.QDeploymentAnnotations, "deployment", func(v topologyVectors) model.Vector { return v.DeploymentAnnotations }},
	{"StatefulSet", promql.QStatefulSetAnnotations, "statefulset", func(v topologyVectors) model.Vector { return v.StatefulSetAnnotations }},
	{"DaemonSet", promql.QDaemonSetAnnotations, "daemonset", func(v topologyVectors) model.Vector { return v.DaemonSetAnnotations }},
	{"ReplicaSet", promql.QReplicaSetAnnotations, "replicaset", func(v topologyVectors) model.Vector { return v.ReplicaSetAnnotations }},
	{"Job", promql.QJobAnnotations, "job_name", func(v topologyVectors) model.Vector { return v.JobAnnotations }},
	{"CronJob", promql.QCronJobAnnotations, "cronjob", func(v topologyVectors) model.Vector { return v.CronJobAnnotations }},
}

// resolveControllerApplications builds the (cluster, namespace, kind, name) →
// ArgoCD Application index from the six controller-annotation families. Each
// family goes through the SAME generic resolveApplications the service and PVC
// resolvers use — same tracking-id label, same segment-before-":" parse, same
// lexically-smallest-raw tie-break, same drop-when-the-Application-would-be-
// empty rule — with a keyOf that stamps the family's constant owner kind.
//
// The six results are merged into one map. Their key spaces are disjoint by
// construction (the kind component differs), so the merge needs no cross-family
// tie-break and is independent of iteration order (D6 determinism).
//
// OPTIONAL: every family is empty unless the operator allowlisted the
// annotation, which is the stock kube-state-metrics state. An empty input
// yields an empty index and no pod resolves an Application.
func resolveControllerApplications(v topologyVectors, mc *clusterResolver) map[controllerKey]string {
	out := map[controllerKey]string{}
	for _, f := range controllerAnnotationFamilies {
		apps := resolveApplications(f.vec(v), argoTrackingIDLabel,
			func(m model.Metric) (controllerKey, bool) {
				name := string(m[f.nameLabel])
				if name == "" {
					return controllerKey{}, false
				}
				return controllerKey{
					cluster:   mc.bucket(f.query, m),
					namespace: string(m["namespace"]),
					kind:      f.kind,
					name:      name,
				}, true
			})
		// Key spaces are disjoint by kind, so a plain copy is order-free.
		maps.Copy(out, apps)
	}
	return out
}

// jobKey identifies a Job by its cluster-scoped namespace/name. It is
// deliberately NOT podNameKey: the two are structurally identical, so sharing
// one type would let a Job key silently satisfy a pod-keyed lookup (and vice
// versa) with no compiler complaint.
type jobKey struct{ cluster, namespace, job string }

// resolveJobCronJobOwners builds the (cluster, namespace, job) → owning CronJob
// name index from kube_job_owner. It exists only for ArgoCD Application
// resolution: the Kubernetes CronJob controller copies only
// spec.jobTemplate.metadata annotations onto the Jobs it creates — never the
// CronJob object's own annotations — so ArgoCD's tracking-id never reaches a
// Job and a CronJob-managed pod can only resolve its Application one level up.
//
// Only controller rows naming a CronJob are retained. Unlike the pre-existing
// rsToDeployment pass in resolvePodOwners, this one DOES filter
// owner_is_controller: new code honours the authoritative label, while
// tightening rsToDeployment would move the pod `owner` attribute and is out of
// scope. On a defensive collision the lexically-smallest CronJob name wins (D6).
//
// This index is NEVER read by resolvePodOwners, which is what makes "the hop
// cannot change data.owner" a structural property rather than a convention.
func resolveJobCronJobOwners(vec model.Vector, mc *clusterResolver) map[jobKey]string {
	out := make(map[jobKey]string, len(vec))
	for _, s := range vec {
		if string(s.Metric["owner_kind"]) != "CronJob" || string(s.Metric["owner_is_controller"]) != "true" {
			continue
		}
		job := string(s.Metric["job_name"])
		cronJob := string(s.Metric["owner_name"])
		if job == "" || cronJob == "" {
			continue
		}
		key := jobKey{mc.bucket(promql.QJobOwner, s.Metric), string(s.Metric["namespace"]), job}
		if cur, ok := out[key]; ok && cur <= cronJob {
			continue
		}
		out[key] = cronJob
	}
	return out
}

// resolvePodApplications builds the (cluster, namespace, pod) → ArgoCD
// Application index by joining each pod's already-resolved controller owner to
// the controller-annotation index. ArgoCD stamps
// `argocd.argoproj.io/tracking-id` on the workload objects it applies, never on
// the pods a controller spawns, so the controller is the only place the value
// exists — no pod-level label is read.
//
// Because the D34 ReplicaSet skip has already collapsed a ReplicaSet owner to
// its Deployment, the Deployment case needs no extra hop. The ONE fallback is
// Job → CronJob: when the owner is a Job that carries no annotation of its own,
// the owning CronJob is consulted. The Job's own annotation is tried FIRST, so
// a Job that ArgoCD manages directly keeps its own Application — the same
// "nearest managed ancestor wins" rule the ReplicaSet collapse implies.
//
// A pod with no controller owner, or an owner of a kind outside
// controllerAnnotationFamilies, is absent from the returned map so the caller
// omits data.application entirely rather than emitting "".
//
// jobAnnotationsDegraded suppresses the hop wholesale. The hop's precondition
// is "the Job carries no annotation of its OWN", and a degraded
// kube_job_annotations cannot establish it: every Job misses, the annotated
// ones included. Taking the hop then would attribute a directly-managed Job's
// pod to its CronJob's Application — the one degrade that SUBSTITUTES a wrong
// value instead of omitting a right one, which no other optional leg does. The
// cost of suppressing is that a genuinely annotation-less Job under an
// annotated CronJob also loses its Application for that build; losing a string
// is strictly better than reporting the wrong one, and it keeps every degrade
// in this package subtractive (harden-controller-annotation-legs D3).
func resolvePodApplications(
	owners map[podNameKey]ownerRef,
	ctrlApps map[controllerKey]string,
	jobCronJobs map[jobKey]string,
	jobAnnotationsDegraded bool,
) map[podNameKey]string {
	out := make(map[podNameKey]string, len(owners))
	src := indexedOwnerApps{ctrlApps: ctrlApps, jobCronJobs: jobCronJobs, jobAnnotationsDegraded: jobAnnotationsDegraded}
	for pod, owner := range owners {
		src.cluster, src.namespace = pod.cluster, pod.namespace
		// The index source never errors.
		if app, _ := resolveOwnerApplication(context.Background(), src, owner.kind, owner.name); app != "" {
			out[pod] = app
		}
	}
	return out
}

// ownerAppSource answers the two questions the controller → Application chain
// asks, for one (cluster, namespace). The build answers them from whole-estate
// indexes; ResolvePodApplication answers them with one upstream query each.
type ownerAppSource interface {
	// controllerApp returns the controller's Application; ok is false when the
	// controller carries no usable tracking-id or its kind has no family.
	controllerApp(ctx context.Context, kind, name string) (app string, ok bool, err error)
	// cronJobOf returns the CronJob controlling a Job; ok is false when none
	// does or when the answer cannot be established.
	cronJobOf(ctx context.Context, job string) (cronJob string, ok bool, err error)
}

// resolveOwnerApplication is the ONE statement of how a pod's (already
// ReplicaSet-collapsed) controller owner resolves an Application: the
// controller's own annotation first, then — for a Job only — its owning
// CronJob's. It returns "" when nothing resolves.
func resolveOwnerApplication[S ownerAppSource](ctx context.Context, src S, kind, name string) (string, error) {
	app, ok, err := src.controllerApp(ctx, kind, name)
	if err != nil || ok || kind != "Job" {
		return app, err
	}
	cronJob, ok, err := src.cronJobOf(ctx, name)
	if err != nil || !ok {
		return "", err
	}
	app, _, err = src.controllerApp(ctx, "CronJob", cronJob)
	return app, err
}

// indexedOwnerApps is the build's ownerAppSource over the resolved indexes,
// scoped to one (cluster, namespace).
type indexedOwnerApps struct {
	ctrlApps               map[controllerKey]string
	jobCronJobs            map[jobKey]string
	jobAnnotationsDegraded bool
	cluster, namespace     string
}

var _ ownerAppSource = indexedOwnerApps{}

// controllerApp answers from the index, so it never errors; the error result is
// the ownerAppSource contract, which podAppLookup satisfies with real queries.
func (s indexedOwnerApps) controllerApp(_ context.Context, kind, name string) (string, bool, error) {
	app, ok := s.ctrlApps[controllerKey{s.cluster, s.namespace, kind, name}]
	return app, ok, nil
}

// cronJobOf reports no CronJob when kube_job_annotations degraded: the hop
// needs that family to establish the Job has no annotation of its own.
func (s indexedOwnerApps) cronJobOf(_ context.Context, job string) (string, bool, error) {
	if s.jobAnnotationsDegraded {
		return "", false, nil
	}
	cronJob, ok := s.jobCronJobs[jobKey{s.cluster, s.namespace, job}]
	return cronJob, ok, nil
}

// resolveServiceApplications builds the (cluster, namespace, service) → ArgoCD
// Application index from kube_service_annotations'
// annotation_argocd_argoproj_io_tracking_id label (KSM's sanitised form of the
// argocd.argoproj.io/tracking-id annotation). OPTIONAL: an absent/empty vector
// yields an empty map (services carry no Application). Deterministic per
// "absent when empty" (lexically-smallest raw tracking-id wins on collision).
func resolveServiceApplications(vec model.Vector, mc *clusterResolver) map[serviceKey]string {
	return resolveApplications(vec, argoTrackingIDLabel, func(m model.Metric) (serviceKey, bool) {
		svc := string(m["service"])
		if svc == "" {
			return serviceKey{}, false
		}
		return serviceKey{mc.bucket(promql.QServiceAnnotations, m), string(m["namespace"]), svc}, true
	})
}

// resolvePVCApplications builds the (cluster, namespace, claim) → ArgoCD
// Application index from kube_persistentvolumeclaim_annotations' tracking-id
// label, keyed identically to resolvePVCInfo so the per-PVC assembly
// can join it. OPTIONAL/graceful and deterministic like the service variant.
func resolvePVCApplications(vec model.Vector, mc *clusterResolver) map[pvcKey]string {
	return resolveApplications(vec, argoTrackingIDLabel, func(m model.Metric) (pvcKey, bool) {
		claim := string(m["persistentvolumeclaim"])
		if claim == "" {
			return pvcKey{}, false
		}
		return pvcKey{mc.bucket(promql.QPVCAnnotations, m), string(m["namespace"]), claim}, true
	})
}

// pvcInheritedApps computes, per PVC ID, the ArgoCD Application a PVC may
// inherit from the pods that mount it (D13): the lexically-smallest non-empty
// Application across all its mounting pods (from the pod-PVC bindings, joined to
// each pod's already-resolved Application via podApp keyed by pod ID). A PVC
// whose mounting pods all carry no Application is absent from the result. The
// accumulation is a pure min over the binding set, so it is independent of
// binding order (D6 determinism). The caller applies this only to PVCs that have
// no Application of their own, so an own annotation always wins.
func pvcInheritedApps(bindings []PodPVCBinding, podApp map[string]string) map[string]string {
	out := make(map[string]string)
	for _, b := range bindings {
		app := podApp[b.PodID]
		if app == "" {
			continue
		}
		if cur, ok := out[b.PVCID]; !ok || app < cur {
			out[b.PVCID] = app
		}
	}
	return out
}

// argoAppName extracts the ArgoCD Application from a tracking-id value: the
// segment before the first ":" (ArgoCD <app>:<group>/<kind>:<ns>/<name> form);
// a value with no ":" is verbatim, an empty leading segment yields "".
func argoAppName(raw string) string {
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// usableTrackingID reports whether raw yields a non-empty Application. Every
// tracking-id pick applies it BEFORE the lexically-smallest comparison: ':'
// sorts below every letter and digit, so a malformed ":apps/..." sibling would
// otherwise win and suppress a valid Application.
func usableTrackingID(raw string) bool {
	return raw != "" && argoAppName(raw) != ""
}

// betterTrackingID reports whether raw should replace best ("" when nothing is
// picked yet): among usable tracking-ids the lexically-smallest raw value wins.
// Every tracking-id pick goes through it.
func betterTrackingID(best, raw string) bool {
	return usableTrackingID(raw) && (best == "" || raw < best)
}

// resolveApplications builds a key → ArgoCD Application index from a vector
// carrying a tracking-id under `label`. For each key it keeps the
// lexically-smallest non-empty raw tracking-id (the tie-break is on the raw
// value, so one map suffices — deterministic), then derives the Application in
// place (argoAppName), dropping keys whose Application is empty so the map stays
// "absent when empty" (never present-but-""). keyOf returns (key, false) to skip
// a series (e.g. missing name label). Shared by the pod / service / PVC
// resolvers, which differ only in their key type, name label, and tracking
// label.
func resolveApplications[K comparable](vec model.Vector, label string, keyOf func(model.Metric) (K, bool)) map[K]string {
	out := make(map[K]string, len(vec))
	for _, s := range vec {
		raw := string(s.Metric[model.LabelName(label)])
		// Filter before keyOf, which tallies missing-cluster samples.
		if !usableTrackingID(raw) {
			continue
		}
		key, ok := keyOf(s.Metric)
		if !ok {
			continue
		}
		if betterTrackingID(out[key], raw) {
			out[key] = raw
		}
	}
	// Every surviving raw has a non-empty Application; derive it in place.
	for key, raw := range out {
		out[key] = argoAppName(raw)
	}
	return out
}
