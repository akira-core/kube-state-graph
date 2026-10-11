package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"
)

// markScopeIssued records that q's by-reference or QoS scope was non-empty
// and at least one chunk reached the upstream. mu MUST be the one *sync.Mutex
// shared by every second-wave goroutine of this read (readTopology creates it
// and threads it through readScopedQoS / readScopedPods / readScopedNodes /
// readScopedControllers): the QoS, pod, node and controller waves run
// concurrently and each may call this for its own families, and Go maps are
// not safe for concurrent writes even to disjoint keys.
func markScopeIssued(v *topologyVectors, mu *sync.Mutex, q promql.Query) {
	mu.Lock()
	defer mu.Unlock()
	if v.ScopeIssued == nil {
		v.ScopeIssued = make(map[promql.Query]bool)
	}
	v.ScopeIssued[q] = true
}

// addExtraSeries adds n to the family's recovery tally. n may be zero: a
// family that was issued and matched nothing is present with 0, which is
// distinct from a family that was never issued.
func addExtraSeries(v *topologyVectors, mu *sync.Mutex, q promql.Query, n int) {
	mu.Lock()
	defer mu.Unlock()
	if v.ExtraSeriesCount == nil {
		v.ExtraSeriesCount = make(map[promql.Query]int)
	}
	v.ExtraSeriesCount[q] += n
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
// (harden-controller-annotation-legs D3) and kube_pod_container_info whose
// cardinality multiplies with containers, image variants and pod churn
// (harden-topology-read-cardinality D1); all three degrade like Harvest.
//
// ReadTopology is the /v1/graph read: every leg, every pod (fullPlan). The
// storage build reads through the same fan-out under storagePlan.
func ReadTopology(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
) (Topology, error) {
	return readTopology(ctx, q, window, end, opts, sel, fullPlan)
}

// readTopology is ReadTopology under an endpoint's read plan: plan decides which
// first-wave legs are issued at all and whether the pod families are read by
// reference (see topologyPlan). Everything else — the fan-out, the error
// semantics, the parse — is shared, so the two endpoints cannot disagree about
// what a pod, a claim or an aggregate is.
func readTopology(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
) (Topology, error) {
	keys := opts.LabelKeys
	// Each goroutine writes a distinct field, so concurrent writes to v are
	// race-free (no overlapping memory); g.Wait() establishes the happens-before
	// edge to the read below.
	var v topologyVectors
	// The parse must derive claim tokens exactly as the scope computation below
	// did, or a claim could be fetched for and then not joined.
	v.VolumeKey = opts.volumeKey()
	// Whether this build is in hub mode: the volume-label family read
	// restricted to the rooted components, and the claim families read FROM
	// those rows. A plan property and a configuration property (the byte
	// budget), so it is decided once, before anything launches, and
	// the launch, the waves and the parse all read the same answer. A storage
	// build has usually resolved it already, to pick its request matchers and
	// routing; resolving is idempotent.
	plan, prepErr := plan.prepare(window, opts.qosScopeBatchBytes(), opts.LabelKeys, sel)
	if prepErr != nil {
		return Topology{}, prepErr
	}
	v.VolumeLabelsRestricted = plan.rootedClaims()
	v.MaterialiseUnboundClaims = plan.claimSeeded()

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
			return wrapQueryError(name, err)
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

	// Second-wave prerequisites. Each is closed when its leg returns, whatever it
	// returns (signalWhenDone), so a scoped wave starts as soon as the families
	// its scope is computed from land instead of behind the slowest
	// kube-state-metrics leg (design D5 of the scoped QoS read).
	pvcInfoDone, volumeLabelsDone, bindingsDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pvcAnnotationsDone := make(chan struct{})
	signals := map[promql.Query]chan struct{}{
		promql.QPVCInfo:        pvcInfoDone,
		promql.QVolumeLabels:   volumeLabelsDone,
		promql.QPVCBindings:    bindingsDone,
		promql.QPVCAnnotations: pvcAnnotationsDone,
	}
	if plan.rootedClaims() || plan.claimSeeded() {
		// The claim families are not beside-seed legs for a Harvest root or a
		// claim root: the claim-keyed reads below close these three channels
		// when they return, and closing them here as well would panic.
		delete(signals, promql.QPVCInfo)
		delete(signals, promql.QPVCBindings)
		delete(signals, promql.QPVCAnnotations)
	}
	if plan.bindingsFromSeed() {
		// The node or pod seed closes bindingsDone. Closing it here as well
		// would panic, and leaving it to the unissued-signal closer would
		// let the pod wave run before the seed's bindings land.
		delete(signals, promql.QPVCBindings)
	}
	if plan.tracksByReference() && !plan.rootedClaims() {
		// The workload expansion (or a claim root's, which shares its token
		// read) closes these when the claim side and the token read return.
		// Closing them here would let QoS run on empty vectors, and closing them
		// twice would panic.
		delete(signals, promql.QPVCInfo)
		delete(signals, promql.QVolumeLabels)
		delete(signals, promql.QPVCAnnotations)
	}
	// svmRows are phase 1's SVM-group rows, kept apart for owner completion.
	// Written by the phase-1 leg before volumeLabelsDone closes.
	var svmRows model.Vector
	legs := topologyLegs(&v)
	start := func(l topologyLeg) {
		var run func() error
		switch {
		case l.query == promql.QVolumeLabels && plan.kind == graph.StorageRootONTAPNode && plan.rootedClaims():
			run = readONTAPNodeVolumeLabels(ctx, q, window, end, opts, sel, plan, l.dst, &svmRows)
		case l.query == promql.QVolumeLabels && plan.rootedClaims():
			// Phase 1 of the rooted read. Its restriction comes from the
			// request, so it runs beside ALERTS. The done-signal also gates
			// the claim read and owner completion.
			run = readRootedVolumeLabels(ctx, q, end, plan.phaseOne, l.dst, &svmRows)
		case !l.optional || plan.failsClosed(l.query):
			run = fetch(l.query, l.dst)
		case l.degraded != nil:
			run = fetchOptionalTracking(l.query, l.dst, l.degraded)
		default:
			run = fetchOptional(l.query, l.dst)
		}
		if done, ok := signals[l.query]; ok {
			run = signalWhenDone(run, done)
			delete(signals, l.query)
		}
		g.Go(run)
	}
	for _, l := range legs {
		if plan.issuesFirstWave(l.query) || plan.issuesBesideSeed(l.query) {
			start(l)
		}
	}
	// A prerequisite this plan never issues can never signal. Closing it lets the
	// wave it gates compute an empty scope and issue nothing, instead of waiting
	// forever.
	for _, done := range signals {
		close(done)
	}

	// scopeMu guards every second-wave write to v.ScopeIssued — QoS, pods,
	// nodes and controllers all run concurrently and each may mark its own
	// families issued. It is a plain local *sync.Mutex, deliberately NOT a
	// field of topologyVectors: that struct is passed BY VALUE into a couple
	// of dozen existing resolver functions, and embedding a lock there would
	// make every one of those calls a lock copy (go vet's copylocks check).
	var scopeMu sync.Mutex

	// The six QoS workload families are NOT issued in the first wave. They are
	// the one leg whose useful population is not known until another leg has
	// been read: ONTAP collects a workload per volume on the filer, and the
	// resolver consults them only for claims that already matched a
	// volume_labels series. readScopedQoS waits on exactly the two families its
	// scope is computed from and then issues each workload query restricted to
	// the FlexVol names those claims actually matched.
	//
	// Under a rooted volume-label read the QoS scope must be computed over the
	// MERGED result, so it waits on the tail (owner completion and phase 2)
	// instead of phase 1: the candidate set the parse sees and the volume scope
	// the QoS wave reads must be the same set.
	//
	// The hub's claim chain runs beside that tail (claimscope.go): the
	// claim-info read waits on phase 1 and closes pvcInfoDone; the claim-name
	// read waits on it and closes bindingsDone and pvcAnnotationsDone — the
	// three channels the first wave closes outside hub mode. Every one closes
	// on every return path, so a failed leg empties the scopes downstream of it
	// instead of blocking them.
	volumeLabelsFinal := volumeLabelsDone
	appDone := make(chan struct{})
	if plan.rootedClaims() {
		var cov hubCoverage
		g.Go(signalWhenDone(func() error {
			return readHubClaimInfo(ctx, q, window, end, opts, sel, &v, &scopeMu, &cov, volumeLabelsDone)
		}, pvcInfoDone))
		g.Go(signalWhenDone(signalWhenDone(func() error {
			return readHubClaimFamilies(ctx, q, window, end, opts, sel, &v, &scopeMu, &cov, pvcInfoDone)
		}, bindingsDone), pvcAnnotationsDone))
		finalDone := make(chan struct{})
		g.Go(signalWhenDone(func() error {
			return readVolumeLabelsTail(ctx, q, window, end, opts, sel, plan, &v, &svmRows,
				volumeLabelsDone, pvcInfoDone)
		}, finalDone))
		volumeLabelsFinal = finalDone
	}
	if plan.byReference {
		if len(plan.applicationRoots) == 0 {
			close(appDone)
		}
	} else {
		close(appDone)
	}
	if plan.claimSeeded() {
		// A claim root seeds exactly the claim-info read, so it takes the hub's
		// claim side — claim-info already loaded, then the claim families by
		// claim, mounters included — and the workload roots' candidate
		// completion: no phase 1 exists, so every loaded claim's token is read.
		// Each of the three channels has one closer here, and readWorkloadClaims
		// stays out: it derives the claim set from bindings, which a claim root
		// does not have before the claim-info read.
		var cov hubCoverage
		g.Go(signalWhenDone(func() error {
			return readClaimSeed(ctx, q, window, end, opts, sel, plan, &v, &scopeMu)
		}, pvcInfoDone))
		g.Go(signalWhenDone(signalWhenDone(func() error {
			return readHubClaimFamilies(ctx, q, window, end, opts, sel, &v, &scopeMu, &cov, pvcInfoDone)
		}, bindingsDone), pvcAnnotationsDone))
		finalDone := make(chan struct{})
		g.Go(signalWhenDone(func() error {
			return readWorkloadVolumeLabels(ctx, q, window, end, opts, sel, plan, &v, &scopeMu, pvcInfoDone)
		}, finalDone))
		volumeLabelsFinal = finalDone
	}
	if plan.tracksByReference() && !plan.rootedClaims() && !plan.claimSeeded() {
		finalDone := make(chan struct{})
		g.Go(signalWhenDone(signalWhenDone(func() error {
			return readWorkloadClaims(ctx, q, window, end, opts, sel, plan, &v, &scopeMu, bindingsDone, appDone)
		}, pvcInfoDone), pvcAnnotationsDone))
		g.Go(signalWhenDone(func() error {
			return readWorkloadVolumeLabels(ctx, q, window, end, opts, sel, plan, &v, &scopeMu, pvcInfoDone)
		}, finalDone))
		volumeLabelsFinal = finalDone
	}
	g.Go(func() error {
		return readScopedQoS(ctx, callerCtx, q, window, end, opts, sel, &v, &scopeMu, plan.failClosed,
			pvcInfoDone, volumeLabelsFinal)
	})
	// Under a by-reference plan, kube_pod_info / kube_pod_owner, the four
	// kube_node_* families and the eight controller-owner /
	// controller-annotation families are three more second waves
	// (scope-controller-legs-by-reference): pods restricted to the pods a
	// claim-binding series names plus the request's pod roots, issued once
	// the binding family has landed; nodes and controllers each restricted to
	// what the LOADED pods name, issued once the pod wave has landed (podsDone
	// closes on every return path of readScopedPods, success or failure, so
	// the two later waves compute an empty scope and issue nothing rather
	// than block forever when the pod wave fails).
	switch plan.kind {
	case graph.StorageRootNode:
		if len(plan.nodeRoots) > 0 {
			g.Go(signalWhenDone(func() error {
				return readNodeSeed(ctx, q, window, end, opts, sel, plan, &v, &scopeMu)
			}, bindingsDone))
		}
	case graph.StorageRootPod:
		if len(plan.pods) > 0 {
			g.Go(signalWhenDone(func() error {
				return readPodSeed(ctx, q, window, end, opts, sel, plan, &v, &scopeMu)
			}, bindingsDone))
		}
	case graph.StorageRootONTAPCluster, graph.StorageRootONTAPNode, graph.StorageRootAggr, graph.StorageRootSVM, graph.StorageRootApplication,
		graph.StorageRootPVC, graph.StorageRootPV:
		// Harvest, application and claim seeds are launched with the expansion, not here.
	}
	flowlessDone := make(chan struct{})
	// Either read launches the goroutine. Every kind reading aggregate gauges
	// reads controllers today, but the launch must not depend on that.
	if plan.flowlessAggrGauges() || plan.flowlessControllers() {
		g.Go(signalWhenDone(func() error {
			return readFlowlessHarvest(ctx, q, window, end, opts, sel, plan, &v, &scopeMu, volumeLabelsFinal)
		}, flowlessDone))
	} else {
		close(flowlessDone)
	}
	if plan.tracksByReference() {
		g.Go(func() error {
			return readReachedHarvest(ctx, q, window, end, opts, sel, plan, &v, &scopeMu, pvcInfoDone, volumeLabelsFinal, flowlessDone)
		})
	}
	if plan.byReference {
		var recovered []podSeriesKey
		if len(plan.applicationRoots) > 0 {
			g.Go(signalWhenDone(signalWhenDone(func() error {
				names, err := readScopedApplications(ctx, q, window, end, opts, sel, plan.applicationRoots, &v, &scopeMu)
				recovered = names
				return err
			}, bindingsDone), appDone))
		}
		podsDone := make(chan struct{})
		g.Go(signalWhenDone(func() error {
			return readScopedPods(ctx, q, window, end, opts, sel, plan.pods, plan.applicationRoots, &v, &scopeMu,
				bindingsDone, appDone, pvcAnnotationsDone, &recovered)
		}, podsDone))
		g.Go(func() error {
			return readScopedNodes(ctx, q, window, end, opts, sel, plan.nodeRoots, &v, &scopeMu, podsDone)
		})
		g.Go(func() error {
			return readScopedControllers(ctx, q, window, end, opts, sel, &v, &scopeMu, podsDone)
		})
	}
	if err := g.Wait(); err != nil {
		return Topology{}, fmt.Errorf("topology fan-out: %w", err)
	}

	t := parseTopology(v, keys)
	t.RawSeriesCount = tallySeries(legs, plan, &v)
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
// that is the kubelet pair alone. The Harvest families DO carry az / env now
// (read-storage-roots-through-volume-hub D13), so Reaches is true for them,
// but an empty volume_labels is also the ordinary state of a deployment with
// no NetApp storage — reporting it would fire this Warn on every filtered
// request of every such deployment, and nothing in the build can tell the
// two apart. They are excluded by an explicit rule below; QVolumeLabels stays
// in the list so the exclusion is a rule rather than an omission.
//
// An OPTIONAL family (Family.Optional — today only FamilyAlerts) is excluded
// on a DIFFERENT axis, and it needs its own rule because Reaches cannot
// express it: ALERTS does carry az / env / namespace, so Reaches is true for
// exactly the dimensions this Warn tests. What makes it wrong to report is
// that an empty alert vector is the HEALTHY estate — the normal, desired
// outcome — not evidence of a labelling mistake. The family is also the one a
// table may legitimately leave unserved, in which case the router hands back
// an empty vector by design. QAlerts stays in the candidate list for the same
// reason QVolumeLabels does: the exclusion is enforced by an explicit rule
// rather than by omission from a list.
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
		if fam, ok := promql.FamilyOf(q); ok && (fam.Optional() || fam == promql.FamilyHarvest) {
			continue
		}
		// Present-and-zero only: a family absent from the tally was never
		// issued (a hub-mode claim family whose claim scope came out empty),
		// so it returned nothing because nothing asked, not because a label
		// is missing.
		if n, issued := raw[string(q)]; issued && n == 0 && sel.Reaches(q) {
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
