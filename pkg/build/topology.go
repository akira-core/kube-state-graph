package build

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// PodPVCBinding records that a pod mounts a specific PVC. The reader emits
// these so the edge builder can wire pod-mounts-pvc.
type PodPVCBinding struct {
	PodID string
	PVCID string
}

// podKey groups pod samples by their cluster-scoped namespace/name. Multiple
// UIDs under one key indicate restarts.
type podKey struct{ cluster, namespace, pod string }

// podObs is one parsed kube_pod_info sample.
type podObs struct {
	uid     string
	nodeID  string
	ts      model.Time
	labels  map[string]string
	nodeRaw string
	podIP   string
}

// serviceKey identifies a Service by its cluster-scoped namespace/name (D29).
type serviceKey struct{ cluster, namespace, service string }

// podNameKey identifies a pod by its cluster-scoped namespace/name. Used
// internally to join an endpointslice `targetref_name` to its backing pod when
// building EndpointsByService (D29).
type podNameKey struct{ cluster, namespace, pod string }

// ServiceObs carries the kube_service_info facts needed to materialise a
// ServiceNode on demand. ClusterIP is retained verbatim — the headless
// sentinel "None" distinguishes a headless service from a ClusterIP one.
type ServiceObs struct {
	ClusterIP string
}

// EndpointObs is one resolved backing pod of a Service (from
// kube_endpointslice_endpoints, joined to topology pods by targetref).
type EndpointObs struct {
	Pod *graph.PodNode
}

// Topology is the typed result of reading kube-state-metrics-style series for
// a single time window across all clusters in scope.
type Topology struct {
	Pods         []*graph.PodNode
	Nodes        []*graph.K8sNode
	PVCs         []*graph.PVCNode
	NetAppAggrs  []*graph.NetAppAggrNode
	NetAppNodes  []*graph.NetAppNode
	StorageEdges []*graph.Edge
	PodPVCs      []PodPVCBinding

	// NetAppInventory is every NetApp entity the Harvest read NAMED, whether or
	// not a claim joined it. It is deliberately wider than NetAppAggrs /
	// NetAppNodes above, which stay join-only so GET /v1/graph is unchanged:
	// the storage-flow graph needs flowless roots (a degraded aggregate serving
	// no claim is a valid answer to "what is on this filer?"), and the only
	// alternative — passing the request's roots into the build — would make the
	// build a function of the request, which both the cache key and the
	// "selectors alone reach the queries" rule forbid.
	//
	// Its size is bounded by the FILER (tens of aggregates, hundreds of SVMs),
	// not by the Kubernetes estate, and a flowless entity costs nothing at
	// projection — it is dropped unless it is a root.
	NetAppInventory NetAppInventory

	// Alerts is the RAW ALERTS vector, carried unparsed. Matching runs against
	// the ASSEMBLED node set — which the service-graph read contributes synth
	// pods to — so it cannot happen at parse time, and holding the vector is
	// what lets Build resolve it at the one point every node exists but the
	// graph is not yet frozen.
	Alerts model.Vector

	// SVMByPVC maps a PVC node id to the SVM its FlexVol lives in, as resolved
	// by hop A. The same value is already stamped on the PVC's `svm` label; it
	// is surfaced here as well because the storage-flow assembler needs the
	// (claim → SVM) edge without re-deriving it from a label, and because a
	// label is a presentation fact while this is a join result.
	SVMByPVC map[string]string

	// PodsByUID indexes every pod in Pods by its raw Kubernetes UID (without
	// the cluster prefix). K8s pod UIDs are UUIDv4 and unique across clusters
	// in practice, so this is the join key the service-graph reader uses to
	// recover the server-side cluster for `pod-calls-pod` edges (the metric
	// only carries the trace-source / client-side `cluster` label).
	//
	// On duplicate UIDs across clusters (data anomaly), the pod with the
	// lexically-smaller cluster-scoped ID wins. This is a deterministic pure
	// function of the data — NOT first-inserted — so the chosen pod (and hence
	// every `pod-calls-pod` edge target resolved through this index) is stable
	// across rebuilds regardless of map-iteration order (D6 determinism).
	PodsByUID map[string]*graph.PodNode

	// D29 connection-string resolution indexes. Built only when KSM exports
	// services / endpointslices (and, for the slice→service join, allowlists
	// the kubernetes.io/service-name label); empty otherwise. These are
	// INDEXES ONLY — ServiceNodes and service-selects-pod edges are
	// materialised on demand by the service-graph reader for referenced
	// services, not emitted wholesale here.
	//
	//   ServicesByNameNS   — (cluster, namespace, service) → cluster_ip facts
	//   EndpointsByService — (cluster, namespace, service) → backing pods
	ServicesByNameNS   map[serviceKey]ServiceObs
	EndpointsByService map[serviceKey][]EndpointObs

	// ServiceApplications indexes (cluster, namespace, service) → ArgoCD
	// Application name (from kube_service_annotations' tracking-id). ServiceNodes
	// are materialised on demand by the service-graph reader, so it consumes this
	// index to set each node's Application (PVC applications are set directly on
	// the PVCNode at topology assembly instead). Empty when the metric is absent.
	ServiceApplications map[serviceKey]string

	ClustersObserved []string // sorted unique cluster values

	// RawSeriesCount records how many series each topology query returned,
	// keyed by query name. Diagnostic only: the build pipeline uses it to
	// enrich the outside-retention error so an operator can tell which
	// upstream metric came back empty (0 series) versus returned rows that
	// were all discarded in Go (count > 0 but parsed 0, e.g. kube_pod_info
	// samples with an empty uid).
	//
	// It is a POST-selector count, not a raw object count. Seven legs carry a
	// fixed selector that pre-filters what the reader would have discarded
	// anyway — kube_job_owner (CronJob controllers only) and the six
	// controller-annotation families (annotated objects only) — so for those a
	// 0 means "nothing matched the fixed selector", never "the collector is
	// off". Every leg is also narrowed by the request's own az/env/cluster/
	// namespace matchers per promql.queryDims. And because
	// kube_replicaset_annotations / kube_job_annotations are fetchOptional, a
	// 0 for those two ALSO covers "the query errored and the leg degraded" —
	// the accompanying `optional topology query failed` Warn is the only thing
	// that separates the two.
	RawSeriesCount map[string]int

	// ClusterIdentities is the identity table the reader composed, handed to
	// the built graph so the projection-level `cluster` filter can recover each
	// identity's RAW component. Nil for an estate that stamps no az/env pair.
	ClusterIdentities map[string]graph.ClusterIdentity

	// clusters is the resolver that composed those identities. Carried so the
	// service-graph reader resolves the trace `cluster` label and the route
	// store's cluster names through the SAME table — a second resolver could
	// hold a different one and the two would silently disagree.
	clusters *clusterResolver
}

// topologyVectors groups the raw result vectors of the topology fan-out. It
// lets parseTopology take one named argument instead of ten positional,
// same-typed model.Vectors that were easy to transpose at the call sites.
type topologyVectors struct {
	Pod         model.Vector
	Node        model.Vector
	Addr        model.Vector
	PVC         model.Vector
	NodeLabels  model.Vector
	Service     model.Vector
	EpEndpoints model.Vector
	EpLabels    model.Vector
	// Pod controller-owner resolution (D34).
	PodOwner        model.Vector
	ReplicaSetOwner model.Vector
	// Job → CronJob resolution, for pod ArgoCD Application resolution ONLY:
	// resolvePodOwners never reads it, so the pod `owner` attribute cannot move.
	JobOwner model.Vector
	// PVC StorageClass name + bound PV name.
	PVCInfo model.Vector
	// Pod container list resolution (name/image per container).
	PodContainerInfo model.Vector
	// K8s node Ready-status resolution (kube_node_status_condition).
	NodeStatus model.Vector
	// Service / PVC ArgoCD Application resolution (annotation tracking-id).
	ServiceAnnotations model.Vector
	PVCAnnotations     model.Vector
	// Pod ArgoCD Application resolution — one annotation family per controller
	// kind kube-state-metrics can describe. ArgoCD stamps its tracking-id on the
	// managed controller, never on the pods it spawns.
	DeploymentAnnotations  model.Vector
	StatefulSetAnnotations model.Vector
	DaemonSetAnnotations   model.Vector
	ReplicaSetAnnotations  model.Vector
	JobAnnotations         model.Vector
	CronJobAnnotations     model.Vector
	// JobAnnotationsDegraded records that the kube_job_annotations leg came back
	// empty BECAUSE THE QUERY FAILED, as opposed to genuinely matching nothing.
	// resolvePodApplications needs the two told apart: its Job → CronJob hop is
	// gated on "this Job carries no annotation of its own", which only a leg that
	// was actually read can establish.
	//
	// kube_replicaset_annotations — the other degrading family — needs no such
	// flag: a bare ReplicaSet has no further ancestor to consult, so a miss
	// resolves no Application either way and the degrade stays subtractive on
	// its own.
	JobAnnotationsDegraded bool

	// VolumeKey derives each claim's Harvest match token from its bound PV
	// name and decides how that token is compared against the stock `volume`
	// label. Like JobAnnotationsDegraded it is a build-scoped FACT rather than
	// a vector, carried here so parseTopology keeps its two-argument shape.
	// Nil means the defaults (`-` → `_`, suffix match).
	VolumeKey *VolumeKeyRewriter
	// NetApp Harvest storage series, in join order (design.md D3):
	// hop A the volume label series (topology), hop B the QoS workload
	// families (I/O), hop C the QoS fixed-policy ceilings.
	VolumeLabels     model.Vector
	QoSReadOps       model.Vector
	QoSWriteOps      model.Vector
	QoSReadLatency   model.Vector
	QoSWriteLatency  model.Vector
	QoSReadData      model.Vector
	QoSWriteData     model.Vector
	QoSPolicyMaxIOPS model.Vector
	QoSPolicyMaxMBps model.Vector
	AggrStatus       model.Vector
	AggrSpaceUsed    model.Vector
	AggrSpaceTotal   model.Vector
	NetAppNodeStatus model.Vector
	// Controller hardware identity (an info series — labels only) and the four
	// system_node performance counters, all matched on (ontap cluster, node)
	// and all read VERBATIM. They resolve the netapp-node data.hardware and
	// data.perf attributes; none of them feeds data.health, which stays the
	// ONTAP-reported NetAppNodeStatus above.
	NetAppNodeLabels       model.Vector
	NetAppNodeCPUBusy      model.Vector
	NetAppNodeTotalOps     model.Vector
	NetAppNodeTotalLatency model.Vector
	NetAppNodeTotalData    model.Vector
	// Kubelet PVC usage.
	KubeletVolumeUsed     model.Vector
	KubeletVolumeCapacity model.Vector
	// Active alerts (the alert overlay). Unlike every other vector here it is
	// NOT consumed by parseTopology: matching needs the ASSEMBLED node set,
	// which only exists after the service-graph read has contributed its synth
	// pods, so the raw vector is carried through to Build untouched.
	Alerts model.Vector
}

// ReadTopology runs the topology queries in parallel and assembles the
// result.
//
// The service / endpointslice queries (D29) are best-effort: an upstream that
// does not export them (older KSM, or KSM started without
// --resources=services,endpointslices) yields empty indexes, and "://"
// connection-string endpoints simply fall back to `external/<label>`.
// Harvest and kubelet legs are OPTIONAL with log-and-continue error
// semantics (a non-NetApp deployment must build cleanly). Existing KSM
// legs keep abort-on-error semantics, except kube_replicaset_annotations
// and kube_job_annotations whose cardinality accumulates with history
// and which degrade like Harvest (harden-controller-annotation-legs D3).
func ReadTopology(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
) (Topology, error) {
	keys := opts.LabelKeys
	// Each goroutine writes a distinct field, so concurrent writes to v are
	// race-free (no overlapping memory); g.Wait() establishes the happens-before
	// edge to the read below.
	var v topologyVectors
	// The parse must derive claim tokens exactly as the scope computation below
	// did, or a claim could be fetched for and then not joined.
	v.VolumeKey = opts.volumeKey()

	// callerCtx is the CALLER's context, captured before errgroup shadows ctx.
	// fetchOptional must distinguish "the caller went away (build timeout /
	// client disconnect)" from "a sibling leg failed and cancelled gctx" — only
	// the former may fail an OPTIONAL leg. Passing the errgroup ctx would make
	// every optional leg fatal whenever any required leg fails, masking the
	// real error. Mirrors ReadServiceGraph, which keeps ctx and gctx apart for
	// exactly this reason.
	callerCtx := ctx

	g, ctx := errgroup.WithContext(ctx)

	// fetch issues one query and stores its result into dst. It captures the
	// errgroup-derived ctx so a failing leg cancels the rest. The closure
	// recovers its own panics: errgroup (x/sync, post-#53757-revert) does NOT
	// propagate goroutine panics to Wait, so an unrecovered panic here would
	// kill the whole process — the HTTP recovery middleware only covers the
	// handler goroutine. Converting to an error keeps the standard
	// build-failure path (sanitised 500, full detail in server logs).
	fetch := func(name promql.Query, dst *model.Vector) func() error {
		return func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(ctx, "panic in topology query",
						"query", string(name),
						"panic", fmt.Sprint(rec),
						"stack", string(debug.Stack()),
					)
					err = fmt.Errorf("panic in %s query: %v", name, rec)
				}
			}()
			out, err := q.Instant(ctx, string(name), promql.Render(name, window, keys, sel), end)
			*dst = out
			return err
		}
	}
	// fetchOptionalTracking is the OPTIONAL-leg twin of fetch: a query error logs
	// and yields an empty vector instead of failing the build. Caller
	// cancellation still fails the group. Used for Harvest, kubelet, and
	// the two accumulating-cardinality annotation families.
	//
	// A non-nil degraded is set when — and only when — an error was swallowed,
	// so a reader that infers something from the family's ABSENCE can tell
	// "read, matched nothing" from "never read". Only kube_job_annotations
	// needs that today; every other optional leg passes nil via fetchOptional.
	fetchOptionalTracking := func(name promql.Query, dst *model.Vector, degraded *bool) func() error {
		return func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(ctx, "panic in optional topology query",
						"query", string(name),
						"panic", fmt.Sprint(rec),
						"stack", string(debug.Stack()),
					)
					err = fmt.Errorf("panic in %s query: %v", name, rec)
				}
			}()
			out, qerr := q.Instant(ctx, string(name), promql.Render(name, window, keys, sel), end)
			if qerr != nil {
				if cerr := optionalQueryFatal(callerCtx, qerr); cerr != nil {
					return cerr
				}
				slog.WarnContext(ctx, "optional topology query failed; continuing with empty vector",
					"query", string(name),
					"error", qerr)
				*dst = nil
				if degraded != nil {
					*degraded = true
				}
				return nil
			}
			*dst = out
			return nil
		}
	}
	// fetchOptional is fetchOptionalTracking for the legs whose degrade needs no
	// downstream signal.
	fetchOptional := func(name promql.Query, dst *model.Vector) func() error {
		return fetchOptionalTracking(name, dst, nil)
	}

	g.Go(fetch(promql.QPodInfo, &v.Pod))
	g.Go(fetch(promql.QNodeInfo, &v.Node))
	g.Go(fetch(promql.QNodeAddresses, &v.Addr))
	g.Go(fetch(promql.QPVCBindings, &v.PVC))
	g.Go(fetch(promql.QNodeLabels, &v.NodeLabels))
	g.Go(fetch(promql.QServiceInfo, &v.Service))
	g.Go(fetch(promql.QEndpointSliceEndpoints, &v.EpEndpoints))
	g.Go(fetch(promql.QEndpointSliceLabels, &v.EpLabels))
	g.Go(fetch(promql.QPodOwner, &v.PodOwner))
	g.Go(fetch(promql.QReplicaSetOwner, &v.ReplicaSetOwner))
	// Signalled, not awaited by a barrier: the scoped QoS read depends on these
	// two legs alone, so it starts as soon as they land instead of behind the
	// slowest kube-state-metrics leg (design D5).
	pvcInfoDone, volumeLabelsDone := make(chan struct{}), make(chan struct{})
	g.Go(signalWhenDone(fetch(promql.QPVCInfo, &v.PVCInfo), pvcInfoDone))
	g.Go(signalWhenDone(fetchOptional(promql.QVolumeLabels, &v.VolumeLabels), volumeLabelsDone))
	g.Go(func() error {
		return readScopedQoS(ctx, callerCtx, q, window, end, opts, &v,
			pvcInfoDone, volumeLabelsDone)
	})
	g.Go(fetch(promql.QPodContainerInfo, &v.PodContainerInfo))
	g.Go(fetch(promql.QNodeStatusCondition, &v.NodeStatus))
	g.Go(fetch(promql.QServiceAnnotations, &v.ServiceAnnotations))
	g.Go(fetch(promql.QPVCAnnotations, &v.PVCAnnotations))
	// The four live-object-count controller-annotation families and
	// kube_job_owner use `fetch`: an upstream fault is rare and fail-fast
	// is the right response. kube_replicaset_annotations and
	// kube_job_annotations use `fetchOptional` — their cardinality
	// accumulates with history (revisionHistoryLimit / Job history limits)
	// and can exceed an upstream series limit in an otherwise ordinary
	// estate; losing an `application` string is never worth failing the
	// whole graph (harden-controller-annotation-legs D3). Caller cancellation
	// still fails the request.
	g.Go(fetch(promql.QJobOwner, &v.JobOwner))
	g.Go(fetch(promql.QDeploymentAnnotations, &v.DeploymentAnnotations))
	g.Go(fetch(promql.QStatefulSetAnnotations, &v.StatefulSetAnnotations))
	g.Go(fetch(promql.QDaemonSetAnnotations, &v.DaemonSetAnnotations))
	g.Go(fetchOptional(promql.QReplicaSetAnnotations, &v.ReplicaSetAnnotations))
	g.Go(fetchOptionalTracking(promql.QJobAnnotations, &v.JobAnnotations, &v.JobAnnotationsDegraded))
	g.Go(fetch(promql.QCronJobAnnotations, &v.CronJobAnnotations))
	// The six QoS workload families are NOT issued here. They are the one leg
	// whose useful population is not known until another leg has been read:
	// ONTAP collects a workload per volume on the filer, and the resolver
	// consults them only for claims that already matched a volume_labels
	// series. readScopedQoS below waits on exactly the two families its scope
	// is computed from and then issues each workload query restricted to the
	// FlexVol names those claims actually matched.
	g.Go(fetchOptional(promql.QQoSPolicyFixedMaxIOPS, &v.QoSPolicyMaxIOPS))
	g.Go(fetchOptional(promql.QQoSPolicyFixedMaxMBps, &v.QoSPolicyMaxMBps))
	g.Go(fetchOptional(promql.QAggrStatus, &v.AggrStatus))
	g.Go(fetchOptional(promql.QAggrSpaceUsed, &v.AggrSpaceUsed))
	g.Go(fetchOptional(promql.QAggrSpaceTotal, &v.AggrSpaceTotal))
	g.Go(fetchOptional(promql.QNetAppNodeStatus, &v.NetAppNodeStatus))
	g.Go(fetchOptional(promql.QNetAppNodeLabels, &v.NetAppNodeLabels))
	g.Go(fetchOptional(promql.QNetAppNodeCPUBusy, &v.NetAppNodeCPUBusy))
	g.Go(fetchOptional(promql.QNetAppNodeTotalOps, &v.NetAppNodeTotalOps))
	g.Go(fetchOptional(promql.QNetAppNodeTotalLatency, &v.NetAppNodeTotalLatency))
	g.Go(fetchOptional(promql.QNetAppNodeTotalData, &v.NetAppNodeTotalData))
	// The alert overlay. Routed through FamilyAlerts, so a table serving that
	// family on no backend issues nothing and every node stays alert-less —
	// the documented normal state, not a degrade.
	g.Go(fetchOptional(promql.QAlerts, &v.Alerts))
	g.Go(fetchOptional(promql.QKubeletVolumeUsedBytes, &v.KubeletVolumeUsed))
	g.Go(fetchOptional(promql.QKubeletVolumeCapacityBytes, &v.KubeletVolumeCapacity))
	if err := g.Wait(); err != nil {
		return Topology{}, fmt.Errorf("topology fan-out: %w", err)
	}

	t := parseTopology(v, keys)
	t.RawSeriesCount = map[string]int{
		string(promql.QPodInfo):                    len(v.Pod),
		string(promql.QNodeInfo):                   len(v.Node),
		string(promql.QNodeAddresses):              len(v.Addr),
		string(promql.QPVCBindings):                len(v.PVC),
		string(promql.QNodeLabels):                 len(v.NodeLabels),
		string(promql.QServiceInfo):                len(v.Service),
		string(promql.QEndpointSliceEndpoints):     len(v.EpEndpoints),
		string(promql.QEndpointSliceLabels):        len(v.EpLabels),
		string(promql.QPodOwner):                   len(v.PodOwner),
		string(promql.QReplicaSetOwner):            len(v.ReplicaSetOwner),
		string(promql.QPVCInfo):                    len(v.PVCInfo),
		string(promql.QPodContainerInfo):           len(v.PodContainerInfo),
		string(promql.QNodeStatusCondition):        len(v.NodeStatus),
		string(promql.QServiceAnnotations):         len(v.ServiceAnnotations),
		string(promql.QPVCAnnotations):             len(v.PVCAnnotations),
		string(promql.QJobOwner):                   len(v.JobOwner),
		string(promql.QDeploymentAnnotations):      len(v.DeploymentAnnotations),
		string(promql.QStatefulSetAnnotations):     len(v.StatefulSetAnnotations),
		string(promql.QDaemonSetAnnotations):       len(v.DaemonSetAnnotations),
		string(promql.QReplicaSetAnnotations):      len(v.ReplicaSetAnnotations),
		string(promql.QJobAnnotations):             len(v.JobAnnotations),
		string(promql.QCronJobAnnotations):         len(v.CronJobAnnotations),
		string(promql.QVolumeLabels):               len(v.VolumeLabels),
		string(promql.QQoSReadOps):                 len(v.QoSReadOps),
		string(promql.QQoSWriteOps):                len(v.QoSWriteOps),
		string(promql.QQoSReadLatency):             len(v.QoSReadLatency),
		string(promql.QQoSWriteLatency):            len(v.QoSWriteLatency),
		string(promql.QQoSReadData):                len(v.QoSReadData),
		string(promql.QQoSWriteData):               len(v.QoSWriteData),
		string(promql.QQoSPolicyFixedMaxIOPS):      len(v.QoSPolicyMaxIOPS),
		string(promql.QQoSPolicyFixedMaxMBps):      len(v.QoSPolicyMaxMBps),
		string(promql.QAggrStatus):                 len(v.AggrStatus),
		string(promql.QAggrSpaceUsed):              len(v.AggrSpaceUsed),
		string(promql.QAggrSpaceTotal):             len(v.AggrSpaceTotal),
		string(promql.QNetAppNodeStatus):           len(v.NetAppNodeStatus),
		string(promql.QNetAppNodeLabels):           len(v.NetAppNodeLabels),
		string(promql.QNetAppNodeCPUBusy):          len(v.NetAppNodeCPUBusy),
		string(promql.QNetAppNodeTotalOps):         len(v.NetAppNodeTotalOps),
		string(promql.QNetAppNodeTotalLatency):     len(v.NetAppNodeTotalLatency),
		string(promql.QNetAppNodeTotalData):        len(v.NetAppNodeTotalData),
		string(promql.QAlerts):                     len(v.Alerts),
		string(promql.QKubeletVolumeUsedBytes):     len(v.KubeletVolumeUsed),
		string(promql.QKubeletVolumeCapacityBytes): len(v.KubeletVolumeCapacity),
	}
	warnSelectorFamilyEmpty(ctx, sel, keys, t.RawSeriesCount)
	return t, nil
}

// warnSelectorFamilyEmpty surfaces the one operator mistake this change makes
// silent: a metric family that does NOT carry the labels the request filters on
// simply matches nothing, and because the default projection keeps only
// connectivity-connected workload, the result can be an empty graph rather than
// a partial one.
//
// The signature is narrow on purpose — kube-state-metrics returned rows, so the
// selector demonstrably matches the deployment's labelling, yet a kubelet
// family came back empty. A family is reported ONLY when a dimension the
// request actually carries reaches it (promql.Selector.Reaches). In practice
// that is the kubelet pair alone: the Harvest families render NO request
// matcher (az only routes them to a backend, env is inert), so Reaches is
// false for every dimension and an empty volume_labels can never be the
// request's doing — reporting it would fire this Warn on every filtered
// request of every non-NetApp deployment. QVolumeLabels stays in the list so
// the contract is enforced by the table rather than by omission.
//
// FamilyAlerts is excluded on a DIFFERENT axis, and it needs its own rule
// because Reaches cannot express it: ALERTS does carry az / env / namespace,
// so Reaches is true for exactly the dimensions this Warn tests. What makes it
// wrong to report is that an empty alert vector is the HEALTHY estate — the
// normal, desired outcome — not evidence of a labelling mistake. The family is
// also the one a table may legitimately leave unserved, in which case the
// router hands back an empty vector by design. QAlerts stays in the candidate
// list for the same reason QVolumeLabels does: the exclusion is enforced by an
// explicit rule rather than by omission from a list.
//
// It is a Warn, not an error, and stays quiet for every unfiltered build.
func warnSelectorFamilyEmpty(ctx context.Context, sel promql.Selector, keys promql.LabelKeys, raw map[string]int) {
	if !sel.Active() || raw[string(promql.QPodInfo)] == 0 {
		return
	}
	var empty []string
	for _, q := range []promql.Query{
		promql.QKubeletVolumeUsedBytes, promql.QKubeletVolumeCapacityBytes, promql.QVolumeLabels,
		promql.QAlerts,
	} {
		if fam, ok := promql.FamilyOf(q); ok && fam == promql.FamilyAlerts {
			continue
		}
		if raw[string(q)] == 0 && sel.Reaches(q) {
			empty = append(empty, string(q))
		}
	}
	if len(empty) == 0 {
		return
	}
	keys = keys.OrDefault()
	slog.WarnContext(ctx, "selector-filtered build: kube-state-metrics matched but another family returned nothing; check that it carries the labels this request filters on",
		"reason", "selector_family_empty",
		"empty_families", empty,
		"az_label", keys.AZ,
		"env_label", keys.Env,
	)
}

// nodeAddrs holds the best (lexically-smallest) address seen per type for one
// (cluster, node). ExternalIP wins over InternalIP regardless of sample order.
type nodeAddrs struct {
	external string
	internal string
}

func (a nodeAddrs) pick() string {
	if a.external != "" {
		return a.external
	}
	return a.internal
}

// volumeKey resolves the derivation this parse uses, adopting the defaults for
// a zero topologyVectors — which is what every hand-built test fixture and
// every embedder that configures nothing passes.
func (v topologyVectors) volumeKey() *VolumeKeyRewriter {
	if v.VolumeKey != nil {
		return v.VolumeKey
	}
	return defaultVolumeKeyRewriter()
}

func parseTopology(v topologyVectors, keys promql.LabelKeys) Topology {
	clusters := map[string]struct{}{}

	// Resolver for the cluster identity every structure below is keyed on, and
	// the per-metric tallies of samples missing the `cluster` label or naming
	// no single identity; both surfaced as one aggregated warn per metric at
	// the end of the parse.
	mc := newClusterResolver(keys)

	// FIRST PASS: build the identity table from the four families that mint
	// cluster-labelled entities. It must complete before ANY bucket call —
	// including the resolve* helpers below — because step 2 of the ladder
	// (adopt) reads it. The other families resolve THROUGH the table and never
	// add to it, so a join input cannot invent a cluster that holds no entity.
	for _, vec := range []model.Vector{v.Pod, v.Node, v.Service, v.PVC} {
		for _, s := range vec {
			mc.observe(s.Metric)
		}
	}

	// Pod controller-owner resolution (D34), with the ReplicaSet skipped to its
	// owning Deployment. Built up-front so the per-pod assembly below can set
	// each pod's typed Owner attribute (never a label).
	podOwners := resolvePodOwners(v.PodOwner, v.ReplicaSetOwner, mc)

	// PVC info resolution (StorageClass name + bound PV name). Built up-front
	// so the per-PVC assembly below can set each PVC's StorageClass (typed
	// data.storageclass, never a label or node) and its `volumename` label
	// (the bound PV name, rooting the Harvest volume join below).
	pvcInfo := resolvePVCInfo(v.PVCInfo, mc)

	// Kubelet PVC usage (used/capacity bytes). Built up-front so assembly
	// can set PVCNode.UsageValue. OPTIONAL — absent series leave usage nil.
	pvcUsage := resolvePVCUsage(v.KubeletVolumeUsed, v.KubeletVolumeCapacity, mc)

	// Pod container list + ArgoCD Application resolution. Both feed typed pod
	// attributes (never labels) set during the per-pod assembly below. The
	// Application is joined from the pod's controller — ArgoCD annotates the
	// managed workload object, never the pods it spawns — reusing the controller
	// owner resolved above, so the Deployment case needs no extra owner hop.
	podContainers := resolvePodContainers(v.PodContainerInfo, mc)
	podApplications := resolvePodApplications(
		podOwners,
		resolveControllerApplications(v, mc),
		resolveJobCronJobOwners(v.JobOwner, mc),
		v.JobAnnotationsDegraded,
	)

	// Service / PVC ArgoCD Application resolution (annotation tracking-id). Built
	// up-front: the PVC index enriches each PVC at the per-PVC assembly below; the
	// service index is threaded into the service-graph reader (service nodes are
	// materialised there, on demand). Both reuse the pod's segment-before-":" parse.
	pvcApplications := resolvePVCApplications(v.PVCAnnotations, mc)
	serviceApplications := resolveServiceApplications(v.ServiceAnnotations, mc)

	// K8s node Ready-status resolution. Built up-front so the per-node assembly
	// below can set each node's typed ReadyStatus attribute (never a label).
	// Keyed (cluster, node) — the same key the node IP / label joins use.
	nodeReady := resolveNodeReadyStatus(v.NodeStatus, mc)

	// Node IP map: (cluster, node-name) -> {ExternalIP, InternalIP}.
	// ExternalIP is preferred at assembly; InternalIP is the fallback for
	// nodes without one (private / NATed node pools). Other address types
	// are ignored even if a wider selector ever leaks them — hostnames must
	// never reach `ipaddress`.
	nodeIPs := map[[2]string]nodeAddrs{}
	for _, s := range v.Addr {
		cluster := mc.bucket(promql.QNodeAddresses, s.Metric)
		nodeName := string(s.Metric["node"])
		typ := string(s.Metric["type"])
		addr := string(s.Metric["address"])
		if addr != "" && (typ == "ExternalIP" || typ == "InternalIP") {
			key := [2]string{cluster, nodeName}
			cur := nodeIPs[key]
			// Deterministic pick: lexically-smallest address wins on duplicate
			// (cluster, node) samples WITHIN each address type, so the emitted
			// IP is a pure function of the data, not upstream vector order
			// (D6 determinism). The external-over-internal preference is
			// applied at node assembly.
			switch typ {
			case "ExternalIP":
				if cur.external == "" || addr < cur.external {
					cur.external = addr
				}
			case "InternalIP":
				if cur.internal == "" || addr < cur.internal {
					cur.internal = addr
				}
			}
			nodeIPs[key] = cur
		}
		clusters[cluster] = struct{}{}
	}

	// K8s node label map: (cluster, node-name) -> labels (with `label_` prefix removed).
	nodeLabels := map[[2]string]map[string]string{}
	for _, s := range v.NodeLabels {
		cluster := mc.bucket(promql.QNodeLabels, s.Metric)
		nodeName := string(s.Metric["node"])
		key := [2]string{cluster, nodeName}
		if _, ok := nodeLabels[key]; !ok {
			nodeLabels[key] = map[string]string{}
		}
		for ln, lv := range s.Metric {
			name := string(ln)
			if !strings.HasPrefix(name, "label_") {
				continue
			}
			lk, val := unflattenLabel(name), string(lv)
			// Deterministic merge: when two series disagree on a key, the
			// lexically-smaller value wins so the emitted label set is a pure
			// function of the data, not upstream vector order (D6 determinism).
			if cur, ok := nodeLabels[key][lk]; !ok || val < cur {
				nodeLabels[key][lk] = val
			}
		}
		clusters[cluster] = struct{}{}
	}

	// K8s nodes. Deduped by (cluster, node): kube_node_info can return multiple
	// series for one node — two KSM scrape targets (HA, or a rollout still inside
	// the last_over_time window) carry different instance/pod target labels, and a
	// kubelet / OS upgrade churns kubelet_version / os_image within the window.
	// Every node attribute below is sourced from (cluster, node)-keyed join maps
	// (nodeLabels / nodeIPs / nodeReady), so duplicate series describe the
	// identical node; collapsing them here (first occurrence wins — the node is a
	// pure function of the order-free join maps, so the winner is deterministic
	// regardless of vector order, D6) keeps same-ID K8sNodes from flooding
	// NewGraph with "duplicate node ID" warnings.
	nodes := make([]*graph.K8sNode, 0, len(v.Node))
	seenNodes := make(map[[2]string]struct{}, len(v.Node))
	for _, s := range v.Node {
		cluster := mc.bucket(promql.QNodeInfo, s.Metric)
		nodeName := string(s.Metric["node"])
		if nodeName == "" {
			continue
		}
		key := [2]string{cluster, nodeName}
		if _, dup := seenNodes[key]; dup {
			continue
		}
		seenNodes[key] = struct{}{}
		labels := map[string]string{}
		for k, v := range nodeLabels[key] {
			labels[k] = v
		}
		// Contract keys win: set AFTER the KSM-derived merge. An operator node
		// label `cluster=...` flattens to label_cluster, and
		// unflattenLabel("label_cluster") == "cluster" — copying it over the
		// contract value would clobber the cluster-scoping every consumer
		// relies on.
		labels["cluster"] = cluster
		var ips []string
		if ip := nodeIPs[key].pick(); ip != "" {
			ips = []string{ip}
		}
		nodes = append(nodes, &graph.K8sNode{
			IDValue:          graph.K8sNodeID(cluster, nodeName),
			NameValue:        nodeName,
			LabelsValue:      labels,
			IPAddressValue:   ips,
			ReadyStatusValue: nodeReady[key],
		})
		clusters[cluster] = struct{}{}
	}

	// Pods (group by (cluster, namespace, pod) for restart handling).
	podGroups := map[podKey][]podObs{}
	for _, s := range v.Pod {
		cluster := mc.bucket(promql.QPodInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		name := string(s.Metric["pod"])
		uid := string(s.Metric["uid"])
		nodeName := string(s.Metric["node"])
		if uid == "" {
			continue
		}
		labels := map[string]string{
			"cluster":   cluster,
			"namespace": ns,
		}
		if nodeName != "" {
			labels["node"] = graph.K8sNodeID(cluster, nodeName)
		}
		podIP := string(s.Metric["pod_ip"])
		k := podKey{cluster, ns, name}
		podGroups[k] = append(podGroups[k], podObs{
			uid:     uid,
			nodeID:  graph.K8sNodeID(cluster, nodeName),
			ts:      s.Timestamp,
			labels:  labels,
			nodeRaw: nodeName,
			podIP:   podIP,
		})
		clusters[cluster] = struct{}{}
	}

	pods := make([]*graph.PodNode, 0, len(v.Pod))
	podsByUID := map[string]*graph.PodNode{}
	podsByNameNS := map[podNameKey]*graph.PodNode{}
	addPodToIndex := func(uid string, pod *graph.PodNode) {
		if uid == "" {
			return
		}
		if existing, dup := podsByUID[uid]; dup {
			slog.Warn("duplicate pod UID across clusters",
				"uid", uid,
				"existing_id", existing.ID(),
				"new_id", pod.ID(),
			)
			// Deterministic dedupe: the lexically-smaller cluster-scoped ID
			// wins so the winner is a pure function of the data, independent of
			// the randomised map-iteration order this runs in (D6 determinism).
			if existing.ID() <= pod.ID() {
				return
			}
		}
		podsByUID[uid] = pod
	}
	for k, group := range podGroups {
		// Newest sample first; pods that churned UIDs within the window collapse
		// to the most recent observation since there is no reliable cross-UID
		// identity link (deleted pods do not back-fill metrics). On equal
		// timestamps (two distinct UIDs scraped at the same step) the
		// lexically-larger UID is the deterministic tie-break, so the canonical
		// pick is a pure function of the data, not vector arrival order (D6).
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].ts != group[j].ts {
				return group[i].ts > group[j].ts
			}
			return group[i].uid > group[j].uid
		})
		// kube-state-metrics emits multiple series per pod-UID as labels evolve
		// during scheduling (e.g. node arrives after the first scrape). Merge
		// labels across same-UID samples — newer values win — so the emitted
		// PodNode reflects the most informative observation. The pod IP lives
		// outside labels and is selected separately below.
		merged := mergeSameUIDLabels(group)
		canonical := group[0]
		// Pod IP is sourced from kube_pod_info.pod_ip. Newest sample wins; if
		// the newest is empty (e.g. arrived before scheduling completed) we
		// fall back to the most recent non-empty observation OF THE CANONICAL
		// UID only — like the label merge above, this is strictly per-UID. A
		// recreated pod (same name, new UID) must not inherit the dead
		// predecessor UID's stale pod_ip.
		var podIP string
		for _, obs := range group {
			if obs.uid == canonical.uid && obs.podIP != "" {
				podIP = obs.podIP
				break
			}
		}
		var ips []string
		if podIP != "" {
			ips = []string{podIP}
		}
		// Resolve the controller owner (ReplicaSet skipped to its Deployment)
		// onto the typed Owner attribute — never into labels. nil when the pod
		// has no controller owner. nk is the pod's (cluster, namespace, name) key
		// shared by the owner / application / container indexes.
		nk := podNameKey(k)
		var owner *graph.Owner
		if o, ok := podOwners[nk]; ok {
			owner = &graph.Owner{Kind: o.kind, Name: o.name}
		}
		canonicalPod := &graph.PodNode{
			IDValue:          graph.PodID(k.cluster, canonical.uid),
			NameValue:        k.pod,
			LabelsValue:      merged[canonical.uid],
			IPAddressValue:   ips,
			OwnerValue:       owner,
			ApplicationValue: podApplications[nk],
			ContainersValue:  podContainers[nk],
		}
		pods = append(pods, canonicalPod)
		addPodToIndex(canonical.uid, canonicalPod)
		podsByNameNS[nk] = canonicalPod
	}

	// PVCs + pod-PVC bindings.
	// Each kube_pod_spec_volumes_persistentvolumeclaims_info series wires one
	// pod to one PVC via (cluster, namespace, pod, persistentvolumeclaim).
	pvcByID := map[string]*graph.PVCNode{}
	pvcs := make([]*graph.PVCNode, 0, len(v.PVC))
	bindingSeen := map[PodPVCBinding]bool{}
	bindings := make([]PodPVCBinding, 0, len(v.PVC))
	canonicalPodUID := map[[3]string]string{}
	for k, group := range podGroups {
		canonicalPodUID[[3]string{k.cluster, k.namespace, k.pod}] = group[0].uid
	}
	for _, s := range v.PVC {
		cluster := mc.bucket(promql.QPVCBindings, s.Metric)
		ns := string(s.Metric["namespace"])
		podName := string(s.Metric["pod"])
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			claim = string(s.Metric["claim_name"])
		}
		if claim == "" {
			continue
		}
		id := graph.PVCID(cluster, ns, claim)
		node, seen := pvcByID[id]
		if !seen {
			attrs := pvcInfo[pvcKey{cluster, ns, claim}]
			labels := map[string]string{"cluster": cluster, "namespace": ns}
			// Bound PV name and NetApp Trident SVM, as additive labels. Each
			// key is set only when its value resolved non-empty — never an
			// empty-string label — and svm is impossible without volumename
			// (the chain is rooted at the PV name). `volumename` (the bound PV)
			// is distinct from the `volume` key below (the pod-spec volume
			// name); both may coexist on one PVC.
			if attrs.volumeName != "" {
				labels["volumename"] = attrs.volumeName
			}
			node = &graph.PVCNode{
				IDValue:           id,
				NameValue:         claim,
				LabelsValue:       labels,
				StorageClassValue: attrs.storageClass,
				ApplicationValue:  pvcApplications[pvcKey{cluster, ns, claim}],
				UsageValue:        pvcUsage[pvcKey{cluster, ns, claim}],
			}
			pvcByID[id] = node
			pvcs = append(pvcs, node)
		}
		// Deterministic pick: the lexically-smallest non-empty volume wins
		// across all samples for this PVC, so the emitted label is a pure
		// function of the data, not upstream vector order (D6 determinism).
		if vol := string(s.Metric["volume"]); vol != "" {
			if cur, ok := node.LabelsValue["volume"]; !ok || vol < cur {
				node.LabelsValue["volume"] = vol
			}
		}
		if podName != "" {
			if uid, ok := canonicalPodUID[[3]string{cluster, ns, podName}]; ok {
				// Dedupe by (PodID, PVCID): one claim mounted via two volume
				// names, a restarted pod, or HA-KSM duplicate series would
				// otherwise emit duplicate pod-mounts-pvc edges sharing one
				// UUIDv5 edge ID.
				b := PodPVCBinding{PodID: graph.PodID(cluster, uid), PVCID: id}
				if !bindingSeen[b] {
					bindingSeen[b] = true
					bindings = append(bindings, b)
				}
			}
		}
		clusters[cluster] = struct{}{}
	}

	// PVC ArgoCD Application inheritance (D13): a PVC with no Application of its
	// own inherits the lexically-smallest Application among the pods that mount
	// it, so an unannotated PVC still nests under its workload's application
	// group. The PVC's own annotation (set above from pvcApplications) ALWAYS
	// wins — this pass only fills app-less PVCs — and runs before graph.NewGraph
	// freezes the nodes. Pure function of the (binding set, pod Applications), so
	// order-independent and byte-stable (D6).
	podAppByID := make(map[string]string, len(pods))
	for _, p := range pods {
		if p.ApplicationValue == "" {
			continue
		}
		// Lexically-smallest wins on the (improbable) same-ID pod collision —
		// the same tie-break as addPodToIndex, so the join key is order-free (D6).
		if cur, ok := podAppByID[p.IDValue]; !ok || p.ApplicationValue < cur {
			podAppByID[p.IDValue] = p.ApplicationValue
		}
	}
	inherited := pvcInheritedApps(bindings, podAppByID)
	for _, pvc := range pvcs {
		if pvc.ApplicationValue == "" {
			if app := inherited[pvc.IDValue]; app != "" {
				pvc.ApplicationValue = app
			}
		}
	}

	// Harvest volume join: SVM label, pvc-to-netapp-aggr edges, demand-driven
	// NetApp aggregate + controller nodes. Rooted at each PVC's volumename.
	claims := make([]pvcVolume, 0, len(pvcs))
	for _, pv := range pvcs {
		if vn := pv.LabelsValue["volumename"]; vn != "" {
			claims = append(claims, pvcVolume{id: pv.IDValue, volumeName: vn})
		}
	}
	netapp := resolveNetAppStorage(claims, v)
	for _, pv := range pvcs {
		if svm := netapp.svmByPVC[pv.IDValue]; svm != "" {
			pv.LabelsValue["svm"] = svm
		}
	}

	// Services (D29). kube_service_info carries cluster_ip; "None" means headless.
	servicesByNameNS := map[serviceKey]ServiceObs{}
	for _, s := range v.Service {
		cluster := mc.bucket(promql.QServiceInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		svc := string(s.Metric["service"])
		if svc == "" {
			continue
		}
		servicesByNameNS[serviceKey{cluster, ns, svc}] = ServiceObs{
			ClusterIP: string(s.Metric["cluster_ip"]),
		}
		clusters[cluster] = struct{}{}
	}

	// EndpointSlice -> owning Service name, via the kubernetes.io/service-name
	// label kube-state-metrics flattens to label_kubernetes_io_service_name
	// (requires the operator to allowlist it; absent -> the slice's endpoints
	// stay unmapped and the service falls back to external/<label> downstream).
	type sliceKey struct{ cluster, namespace, slice string }
	sliceToService := map[sliceKey]string{}
	for _, s := range v.EpLabels {
		cluster := mc.bucket(promql.QEndpointSliceLabels, s.Metric)
		ns := string(s.Metric["namespace"])
		slice := string(s.Metric["endpointslice"])
		svc := string(s.Metric["label_kubernetes_io_service_name"])
		if slice == "" || svc == "" {
			continue
		}
		sliceToService[sliceKey{cluster, ns, slice}] = svc
		clusters[cluster] = struct{}{}
	}

	// EndpointsByService: resolve each endpoint's backing pod via
	// (cluster, targetref_namespace, targetref_name) against the loaded pods,
	// keyed by the owning service recovered from the slice->service map. This is
	// the source of the Service → backing-pod fan-out (service-selects-pod edges).
	endpointsByService := map[serviceKey][]EndpointObs{}
	for _, s := range v.EpEndpoints {
		cluster := mc.bucket(promql.QEndpointSliceEndpoints, s.Metric)
		ns := string(s.Metric["namespace"])
		slice := string(s.Metric["endpointslice"])
		svc, ok := sliceToService[sliceKey{cluster, ns, slice}]
		if !ok {
			continue
		}
		if kind := string(s.Metric["targetref_kind"]); kind != "" && kind != "Pod" {
			continue
		}
		targetNS := string(s.Metric["targetref_namespace"])
		if targetNS == "" {
			targetNS = ns
		}
		targetName := string(s.Metric["targetref_name"])
		if targetName == "" {
			continue
		}
		pod, ok := podsByNameNS[podNameKey{cluster, targetNS, targetName}]
		if !ok {
			continue
		}
		key := serviceKey{cluster, ns, svc}
		endpointsByService[key] = append(endpointsByService[key], EndpointObs{Pod: pod})
		clusters[cluster] = struct{}{}
	}

	clusterList := make([]string, 0, len(clusters))
	for c := range clusters {
		clusterList = append(clusterList, c)
	}
	sort.Strings(clusterList)

	mc.warn()

	return Topology{
		Pods:                pods,
		Nodes:               nodes,
		PVCs:                pvcs,
		NetAppAggrs:         netapp.aggrs,
		NetAppNodes:         netapp.nodes,
		StorageEdges:        netapp.edges,
		NetAppInventory:     netapp.inventory,
		SVMByPVC:            netapp.svmByPVC,
		Alerts:              v.Alerts,
		PodPVCs:             bindings,
		PodsByUID:           podsByUID,
		ServicesByNameNS:    servicesByNameNS,
		EndpointsByService:  endpointsByService,
		ServiceApplications: serviceApplications,
		ClustersObserved:    clusterList,
		ClusterIdentities:   mc.snapshot(),
		clusters:            mc,
	}
}

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
		if cur, ok := owners[key]; ok {
			if kind > cur.kind || (kind == cur.kind && name >= cur.name) {
				continue
			}
		}
		owners[key] = ownerRef{kind, name}
	}
	return owners
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
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Name != list[j].Name {
				return list[i].Name < list[j].Name
			}
			return list[i].Image < list[j].Image
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
		apps := resolveApplications(f.vec(v), "annotation_argocd_argoproj_io_tracking_id",
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
	for pod, owner := range owners {
		key := controllerKey{pod.cluster, pod.namespace, owner.kind, owner.name}
		if app, ok := ctrlApps[key]; ok {
			out[pod] = app
			continue
		}
		// Job → CronJob: the only hop, and only on a miss the Job family was
		// actually read to establish.
		if owner.kind != "Job" || jobAnnotationsDegraded {
			continue
		}
		cronJob, ok := jobCronJobs[jobKey{pod.cluster, pod.namespace, owner.name}]
		if !ok {
			continue
		}
		if app, ok := ctrlApps[controllerKey{pod.cluster, pod.namespace, "CronJob", cronJob}]; ok {
			out[pod] = app
		}
	}
	return out
}

// resolveServiceApplications builds the (cluster, namespace, service) → ArgoCD
// Application index from kube_service_annotations'
// annotation_argocd_argoproj_io_tracking_id label (KSM's sanitised form of the
// argocd.argoproj.io/tracking-id annotation). OPTIONAL: an absent/empty vector
// yields an empty map (services carry no Application). Deterministic per
// "absent when empty" (lexically-smallest raw tracking-id wins on collision).
func resolveServiceApplications(vec model.Vector, mc *clusterResolver) map[serviceKey]string {
	return resolveApplications(vec, "annotation_argocd_argoproj_io_tracking_id", func(m model.Metric) (serviceKey, bool) {
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
	return resolveApplications(vec, "annotation_argocd_argoproj_io_tracking_id", func(m model.Metric) (pvcKey, bool) {
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
		// Skip a value whose derived Application would be empty — an empty
		// tracking-id, or an empty leading segment like ":apps/..." — BEFORE the
		// min-pick. Otherwise a malformed sibling could win the lexically-smallest
		// race (':' = 0x3A sorts below every letter/digit) and suppress a valid
		// Application for the same key. Among the surviving (non-empty-app) series
		// the smallest raw tracking-id still wins (the documented tie-break).
		if raw == "" || argoAppName(raw) == "" {
			continue
		}
		key, ok := keyOf(s.Metric)
		if !ok {
			continue
		}
		if cur, ok := out[key]; !ok || raw < cur {
			out[key] = raw
		}
	}
	// Every surviving raw has a non-empty Application; derive it in place.
	for key, raw := range out {
		out[key] = argoAppName(raw)
	}
	return out
}

// pvcKey identifies a PVC by its cluster-scoped namespace/name for the
// StorageClass join. The claim component matches the binding metric's
// persistentvolumeclaim / claim_name and the info metric's
// persistentvolumeclaim.
type pvcKey struct{ cluster, namespace, claim string }

// pvcInfoAttrs carries the per-PVC values read off
// kube_persistentvolumeclaim_info: the StorageClass name (drives the
// pvc-to-storageclass edge) and the bound PersistentVolume name from the
// `volumename` label (surfaced as the PVC `volumename` label and rooting the
// NetApp Trident svm join chain). The zero value means "nothing resolved".
type pvcInfoAttrs struct {
	storageClass string
	volumeName   string
}

// resolvePVCInfo builds the (cluster, namespace, persistentvolumeclaim) →
// {storageclass, volumename} index from kube_persistentvolumeclaim_info. The
// result enriches PVC nodes that already exist (from the pod→PVC binding
// metric); it never materialises a PVC on its own.
//
// The two fields are resolved per-field independently — a series may carry
// `volumename` without `storageclass` and vice versa, and an empty value never
// masks a populated sibling series. OPTIONAL: an absent or empty vector yields
// an empty map and PVCs carry no StorageClass and no volumename (graceful
// degradation). The returned map is a deterministic function of the input
// vector — on a duplicate (cluster, namespace, claim) the lexically-smallest
// non-empty value wins PER FIELD, so the emitted grouping and labels are
// stable across rebuilds (D6 determinism). The only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolvePVCInfo(vec model.Vector, mc *clusterResolver) map[pvcKey]pvcInfoAttrs {
	pick := func(cur *string, val string) {
		if val == "" {
			return
		}
		if *cur == "" || val < *cur {
			*cur = val
		}
	}
	out := make(map[pvcKey]pvcInfoAttrs, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QPVCInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			continue
		}
		key := pvcKey{cluster, ns, claim}
		attrs := out[key]
		pick(&attrs.storageClass, string(s.Metric["storageclass"]))
		pick(&attrs.volumeName, string(s.Metric["volumename"]))
		out[key] = attrs
	}
	return out
}

// readyStatusFromLabel maps a kube_node_status_condition `status` label to the
// graph ReadyStatus value. The caller canonicalises casing to lowercase first
// (the contract does not pin status-label casing — stock KSM lowercases, a
// raw-enum exporter emits "True"/"False"/"Unknown"), so this matches the
// lowercase forms. Any other value yields "" so a malformed status never
// surfaces as a non-enum attribute — the caller drops it (omit ready_status).
func readyStatusFromLabel(status string) string {
	switch status {
	case "true":
		return graph.ReadyStatusReady
	case "false":
		return graph.ReadyStatusNotReady
	case "unknown":
		return graph.ReadyStatusUnknown
	}
	return ""
}

// resolveNodeReadyStatus builds the (cluster, node) → Ready-status index from
// kube_node_status_condition. For condition="Ready", kube-state-metrics emits
// one series per status (true/false/unknown) with value 1 for the active one;
// the reader reads the active row's `status` label and maps it to
// ReadyStatusReady / ReadyStatusNotReady / ReadyStatusUnknown.
//
// The condition="Ready" selector is applied at the query layer; the condition
// guard here is defensive against a wider selector. Only rows with value == 1
// AND a recognised status are considered, so a 0-valued (inactive) row, or a
// malformed status, never wins. On the defensive case where more than one row
// is active for the same (cluster, node) — which correct KSM never emits — the
// lexically-smallest `status` label wins, so the result is order-free (D6).
//
// OPTIONAL: an absent or empty vector yields an empty map and nodes carry no
// ready_status (graceful degradation). "" (absent) is intentionally distinct
// from ReadyStatusUnknown (kubelet lost contact). The returned map is a
// deterministic function of the input vector; the only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolveNodeReadyStatus(vec model.Vector, mc *clusterResolver) map[[2]string]string {
	// (cluster, node) → lexically-smallest active, recognised `status` label.
	raw := make(map[[2]string]string, len(vec))
	for _, s := range vec {
		if string(s.Metric["condition"]) != "Ready" {
			continue
		}
		if s.Value != 1 {
			continue
		}
		// Canonicalise casing at the read site so the guard, the lexical
		// tie-break below, and the final mapping all operate on one casing. The
		// `status` value casing is NOT pinned by the KSM-shaped contract: stock
		// kube-state-metrics lowercases it (addConditionMetrics → strings.ToLower),
		// but an exporter that re-publishes the raw Kubernetes v1.ConditionStatus
		// enum verbatim emits "True"/"False"/"Unknown" — both must resolve.
		status := strings.ToLower(string(s.Metric["status"]))
		if readyStatusFromLabel(status) == "" {
			continue
		}
		cluster := mc.bucket(promql.QNodeStatusCondition, s.Metric)
		node := string(s.Metric["node"])
		if node == "" {
			continue
		}
		key := [2]string{cluster, node}
		if cur, ok := raw[key]; ok && cur <= status {
			continue
		}
		raw[key] = status
	}

	out := make(map[[2]string]string, len(raw))
	for key, status := range raw {
		out[key] = readyStatusFromLabel(status)
	}
	return out
}

// unflattenLabel inverts kube-state-metrics' `label_*` flattening.
//
// Examples:
//
//	"label_topology_kubernetes_io_zone" -> "topology.kubernetes.io/zone"
//	"label_kubernetes_io_arch"          -> "kubernetes.io/arch"
//	"label_app"                          -> "app"
//
// Heuristic: strip the `label_` prefix, then convert underscores to dots
// except the underscore preceding the LAST segment, which becomes a slash if
// the label key contains a domain prefix.
func unflattenLabel(flattened string) string {
	s := strings.TrimPrefix(flattened, "label_")
	// kube-state-metrics replaces invalid label-name characters with `_`.
	// We can't perfectly invert that, but the dominant case is
	// `<dns-prefix>/<segment>` where the prefix uses dots. We approximate:
	// replace all `_` with `.`, then turn the last `.` into `/` if any prior
	// `.` exists.
	withDots := strings.ReplaceAll(s, "_", ".")
	if i := strings.LastIndex(withDots, "."); i > 0 && strings.Contains(withDots[:i], ".") {
		return withDots[:i] + "/" + withDots[i+1:]
	}
	return withDots
}

// mergeSameUIDLabels returns one label map per UID, formed by merging labels
// from every sample with that UID. group is assumed sorted newest-first; older
// samples fill in keys the newer ones omit. This handles kube-state-metrics
// emitting multiple kube_pod_info series per UID as state evolves (e.g. node
// arrives on a later scrape).
func mergeSameUIDLabels(group []podObs) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, obs := range group {
		merged, ok := out[obs.uid]
		if !ok {
			merged = map[string]string{}
			out[obs.uid] = merged
		}
		for k, v := range obs.labels {
			if v == "" {
				continue
			}
			if _, present := merged[k]; !present {
				merged[k] = v
			}
		}
	}
	return out
}
