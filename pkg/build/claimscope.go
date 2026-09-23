package build

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The volume hub's claim-keyed reads (read-storage-roots-through-volume-hub).
//
// A hub-mode /v1/storage-graph build reads the claim side of the chain FROM
// the rooted Harvest rows instead of scanning the zone:
//
//	phase-1 volume_labels rows
//	  → PV candidates                   (pvCandidates, pure)
//	  → kube_persistentvolumeclaim_info {volumename=~candidates}
//	  → bindings / claim annotations / kubelet ×2 {persistentvolumeclaim=~claims}
//	  → pods → Kubernetes nodes and controllers (the existing by-reference waves)
//
// Candidate extraction is a GENERATOR, never a judge. A candidate that names no
// PersistentVolume loads nothing, and whether a loaded claim lands on a FlexVol
// — and on which aggregate, SVM and controller — is still decided solely by the
// configured forward derivation and join (volumekey.go, netapp.go), exactly as
// in every other build. The operator's rewrite rules are an ordered list of
// regexps and are not invertible, so extraction ignores them: a custom rule set
// can only make the hub find fewer claims, never attach a wrong one.

// claimTargets are the five claim-keyed families a hub-mode build reads by
// reference, with the slot each lands in. They are exactly
// promql.ClaimScopedQueries.
func claimTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QPVCInfo, &v.PVCInfo},
		{promql.QPVCBindings, &v.PVC},
		{promql.QPVCAnnotations, &v.PVCAnnotations},
		{promql.QKubeletVolumeUsedBytes, &v.KubeletVolumeUsed},
		{promql.QKubeletVolumeCapacityBytes, &v.KubeletVolumeCapacity},
	}
}

// pvCandidatePrefix is the provisioner-independent head of a dynamically
// provisioned PersistentVolume's name (`pvc-<claim UID>`) as it appears inside
// an ONTAP FlexVol name, which admits only letters, digits and `_`.
const pvCandidatePrefix = "pvc_"

// pvCandidates derives the PersistentVolume names the given FlexVol names may
// embed: for every position at which a name continues with `pvc_` and which is
// the start of the name or follows a `_`, the remainder of the name from that
// position with every `_` replaced by `-`. Sorted and de-duplicated.
//
//	trident_pvc_ab12_cd34   → pvc-ab12-cd34
//	pvc_ab12_cd34           → pvc-ab12-cd34            (empty storagePrefix)
//	x_pvc_pool_pvc_ab12     → pvc-pool-pvc-ab12, pvc-ab12
//	trident_pvc_ab12_clone  → pvc-ab12-clone           (names no PV: loads nothing)
//	vol0, svm_shop_root     → (none)
//
// The match is case-sensitive: a PV name is DNS-1123, so `PVC_` can never be
// the head of one. It is deliberately lenient otherwise — no UUID shape is
// required — because an over-generated candidate costs one alternation branch
// and loads nothing, while a strict shape would buy nothing and miss every PV
// not named by a UUID.
func pvCandidates(volumes []string) []string {
	var out []string
	for _, v := range volumes {
		for i := 0; i+len(pvCandidatePrefix) <= len(v); i++ {
			if i > 0 && v[i-1] != '_' {
				continue
			}
			if strings.HasPrefix(v[i:], pvCandidatePrefix) {
				out = append(out, strings.ReplaceAll(v[i:], "_", "-"))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// volumeNames is the sorted, de-duplicated set of non-empty `volume` labels in
// a volume-label vector.
func volumeNames(rows model.Vector) []string {
	out := make([]string, 0, len(rows))
	for _, s := range rows {
		if v := string(s.Metric[promql.HarvestVolumeLabel]); v != "" {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// maxHubClaimChunks bounds how many queries one claim-keyed family's scope may
// become. Past it the family is read ONCE with no restriction — its fixed
// selector and the request matchers only — and its rows are filtered in the
// reader to the same scope (read-storage-roots-through-volume-hub D6).
//
// Unlike the phase-1 root set, these scopes are data-derived and can be large:
// `ontap_cluster=` on a filer serving tens of thousands of claims yields as
// many candidates. One wide read of a one-series-per-claim family is cheaper
// than dozens of chunk round-trips, and the body is identical either way,
// because the reader keeps exactly the rows the restriction would have
// admitted.
const maxHubClaimChunks = scopeConcurrency

// claimFamily is one claim-keyed family a hub read issues: its scope on the
// family's scopedLabel, and keep, the reader-side filter applied to whatever
// the query returned — the restriction's own predicate, plus the claim-key
// filter of the four claim-name families.
type claimFamily struct {
	query promql.Query
	dst   *model.Vector
	scope []string
	keep  func(model.Metric) bool
}

// issueClaimKeyed issues each family restricted to its scope — chunked under
// the shared byte budget, or once unrestricted past maxHubClaimChunks — and
// then keeps only the rows keep admits. A family whose scope is empty is not
// issued and not tallied. The first query error fails the build: the hub runs
// only on /v1/storage-graph, which fails closed.
func issueClaimKeyed(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	fams []claimFamily,
) error {
	var scoped []scopedFamily
	var wide []claimFamily
	for _, f := range fams {
		if len(f.scope) == 0 {
			continue
		}
		if chunks := promql.ChunkScope(f.scope, opts.qosScopeBatchBytes()); len(chunks) > maxHubClaimChunks {
			slog.DebugContext(ctx, "hub claim scope unbounded; reading the family unrestricted and filtering",
				"query", string(f.query),
				"scope", len(f.scope),
				"chunks", len(chunks),
				"max_chunks", maxHubClaimChunks)
			wide = append(wide, f)
			continue
		}
		scoped = append(scoped, scopedFamily{query: f.query, dst: f.dst, scope: f.scope})
	}

	g, gctx := errgroup.WithContext(ctx)
	if len(scoped) > 0 {
		g.Go(func() (err error) {
			// Chunking runs here, outside issueScopedChunk's per-query recover.
			defer recoverScopedPanic(gctx, scoped[0].query, &err)
			return issueScopedFamilies(gctx, q, window, end, opts, sel, v, scopeMu, scoped)
		})
	}
	for _, f := range wide {
		g.Go(func() error {
			out, err := issueUnrestricted(gctx, q, f.query, window, end, opts.LabelKeys, sel)
			if err != nil {
				return err
			}
			*f.dst = out
			markScopeIssued(v, scopeMu, f.query)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for _, f := range fams {
		if len(f.scope) > 0 {
			*f.dst = keepRows(*f.dst, f.keep)
		}
	}
	return nil
}

// keepRows returns the rows keep admits, in order. It allocates a new vector
// rather than filtering in place, so a vector shared with a fixture is never
// mutated.
func keepRows(rows model.Vector, keep func(model.Metric) bool) model.Vector {
	out := make(model.Vector, 0, len(rows))
	for _, s := range rows {
		if keep(s.Metric) {
			out = append(out, s)
		}
	}
	return out
}

// hubCoverage carries the claim read's counts to the Debug summary the
// claim-family read logs. Written by readHubClaimInfo before pvcInfoDone
// closes, read by readHubClaimFamilies after it has observed that close.
type hubCoverage struct {
	volumes, candidates int
}

// readHubClaimInfo reads kube_persistentvolumeclaim_info restricted on
// `volumename` to the PV candidates the phase-1 volume-label rows yield. It
// waits on phase 1 alone, and reads v.VolumeLabels BEFORE owner completion or
// phase 2 are merged into it (readVolumeLabelsTail waits on this read's
// done-channel first), so only the rooted rows name claims.
//
// No candidate issues nothing: the body then holds the rooted storage entities
// and no path. Both whole-hub misses that were silent before the hub existed
// are logged: rooted volumes that embed no `pvc_` (a Trident nameTemplate, a
// custom volume-name prefix), and candidates that name no claim the build can
// reach.
func readHubClaimInfo(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	cov *hubCoverage,
	phaseOneDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QPVCInfo, &err)
	select {
	case <-phaseOneDone:
	case <-ctx.Done():
		return nil
	}

	vols := volumeNames(v.VolumeLabels)
	cands := pvCandidates(vols)
	cov.volumes, cov.candidates = len(vols), len(cands)
	if len(cands) == 0 {
		if len(vols) > 0 {
			slog.WarnContext(ctx, "storage_root_claim_miss",
				"reason", "no_pv_candidate",
				"volumes", len(vols))
		}
		return nil
	}
	want := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		want[c] = struct{}{}
	}
	if err := issueClaimKeyed(ctx, q, window, end, opts, sel, v, scopeMu, []claimFamily{{
		query: promql.QPVCInfo,
		dst:   &v.PVCInfo,
		scope: cands,
		keep: func(m model.Metric) bool {
			_, ok := want[string(m[promql.VolumeNameLabel])]
			return ok
		},
	}}); err != nil {
		return err
	}
	if len(v.PVCInfo) == 0 {
		// Under a cluster= / namespace= filter an empty claim read is the
		// filter's ordinary outcome — the rooted filer serves other namespaces
		// — not a whole-hub miss, so it is not worth an operator's attention.
		level := slog.LevelWarn
		if sel.Active() {
			level = slog.LevelDebug
		}
		slog.Log(ctx, level, "storage_root_claim_miss",
			"reason", "no_claim",
			"candidates", len(cands),
			"selector_active", sel.Active())
	}
	return nil
}

// hubClaimKey is one claim as the hub's claim-info read names it. The zone and
// environment labels are part of it because a hub-mode read spans every zone
// and environment: a claim name is unique only per namespace, and a raw
// cluster name is reused across zones, so (az, env, cluster) is the claim's
// cluster identity — the key every structure of the parse is built on. The
// cluster is bucketed exactly as the parse buckets it (bucketCluster): an
// absent label and a literal `unknown` are one cluster there, so they must be
// one here, or the filter would drop a row the parse joins.
type hubClaimKey struct {
	az, env, cluster, namespace, claim string
}

func hubClaimKeyOf(m model.Metric, keys promql.LabelKeys, claim string) hubClaimKey {
	return hubClaimKey{
		az:        string(m[model.LabelName(keys.AZ)]),
		env:       string(m[model.LabelName(keys.Env)]),
		cluster:   bucketCluster(string(m["cluster"])),
		namespace: string(m["namespace"]),
		claim:     claim,
	}
}

// readHubClaimFamilies reads the claim-binding family,
// kube_persistentvolumeclaim_annotations and the two kubelet volume-stats
// families restricted on `persistentvolumeclaim` to the claims the claim-info
// read returned, and keeps only the rows whose claim IS one of them.
//
// The filter runs before the pod scope is computed and before the parse: a
// claim name is unique per namespace only, so the restriction also returns
// same-named claims of other namespaces and clusters, and such a binding would
// otherwise load a pod and build a PVC node the rooted filer does not serve.
// Bindings are keyed on `persistentvolumeclaim` alone, never on `claim_name`,
// so the filter admits exactly what the restriction can (an exporter labelling
// only `claim_name` is outside the documented contract).
//
// No claim issues nothing — and therefore no pod, Kubernetes-node or
// controller query either.
func readHubClaimFamilies(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	cov *hubCoverage,
	pvcInfoDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QPVCBindings, &err)
	select {
	case <-pvcInfoDone:
	case <-ctx.Done():
		return nil
	}

	keys := opts.LabelKeys.OrDefault()
	claims := make(map[hubClaimKey]struct{}, len(v.PVCInfo))
	names := make([]string, 0, len(v.PVCInfo))
	for _, s := range v.PVCInfo {
		claim := string(s.Metric[promql.ClaimLabel])
		if claim == "" {
			continue
		}
		claims[hubClaimKeyOf(s.Metric, keys, claim)] = struct{}{}
		names = append(names, claim)
	}
	names = sortedNames(names)
	defer func() {
		slog.DebugContext(ctx, "storage graph volume hub",
			"volumes", cov.volumes,
			"candidates", cov.candidates,
			"claims", len(claims),
			"bindings", len(v.PVC))
	}()
	if len(names) == 0 {
		return nil
	}
	isLoadedClaim := func(m model.Metric) bool {
		_, ok := claims[hubClaimKeyOf(m, keys, string(m[promql.ClaimLabel]))]
		return ok
	}
	fams := make([]claimFamily, 0, len(promql.ClaimScopedQueries)-1)
	for _, t := range claimTargets(v)[1:] {
		fams = append(fams, claimFamily{query: t.query, dst: t.dst, scope: names, keep: isLoadedClaim})
	}
	return issueClaimKeyed(ctx, q, window, end, opts, sel, v, scopeMu, fams)
}

// recoverScopedPanic converts a panic in a hub or rooted volume-label read into
// the build's error, for the reason fetch recovers its own: errgroup does not
// propagate a goroutine panic to Wait, and scope computation runs outside any
// per-query recover. Defer it in EVERY goroutine that computes a scope: a
// recover only catches a panic raised on its own goroutine.
func recoverScopedPanic(ctx context.Context, name promql.Query, err *error) {
	if rec := recover(); rec != nil {
		slog.ErrorContext(ctx, "panic in scoped topology read",
			"query", string(name),
			"panic", fmt.Sprint(rec),
			"stack", string(debug.Stack()),
		)
		*err = fmt.Errorf("panic in %s query: %v", name, rec)
	}
}
