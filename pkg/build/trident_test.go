package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// --- fixture helpers -------------------------------------------------------

func pvcBindingSample(cluster, ns, pod, claim string) model.Sample {
	return model.Sample{Metric: model.Metric{
		"cluster": model.LabelValue(cluster), "namespace": model.LabelValue(ns),
		"pod": model.LabelValue(pod), "persistentvolumeclaim": model.LabelValue(claim),
	}}
}

func pvcInfoSample(cluster, ns, claim, sc, volumeName string) model.Sample {
	m := model.Metric{
		"cluster": model.LabelValue(cluster), "namespace": model.LabelValue(ns),
		"persistentvolumeclaim": model.LabelValue(claim),
	}
	if sc != "" {
		m["storageclass"] = model.LabelValue(sc)
	}
	if volumeName != "" {
		m["volumename"] = model.LabelValue(volumeName)
	}
	return model.Sample{Metric: m}
}

func tridentVolumeSample(cluster, name, backendUUID string) model.Sample {
	m := model.Metric{"name": model.LabelValue(name), "backendUUID": model.LabelValue(backendUUID)}
	if cluster != "" {
		m["cluster"] = model.LabelValue(cluster)
	}
	return model.Sample{Metric: m}
}

func tridentBackendSample(cluster, backendUUID, svm string) model.Sample {
	m := model.Metric{"backendUUID": model.LabelValue(backendUUID), "svm": model.LabelValue(svm)}
	if cluster != "" {
		m["cluster"] = model.LabelValue(cluster)
	}
	return model.Sample{Metric: m}
}

// --- resolvePVCInfo --------------------------------------------------------

// TestResolvePVCInfo_PerFieldIndependence — a series may carry volumename
// without storageclass and vice versa; the two fields resolve independently
// and an empty value never masks a populated sibling series.
func TestResolvePVCInfo_PerFieldIndependence(t *testing.T) {
	t.Parallel()
	out := resolvePVCInfo(sampleVec(
		pvcInfoSample("c", "n", "claim-vol-only", "", "pvc-9f3a"),
		pvcInfoSample("c", "n", "claim-sc-only", "gp3", ""),
		// Two sibling series for one claim, each populating one field.
		pvcInfoSample("c", "n", "claim-split", "gp2", ""),
		pvcInfoSample("c", "n", "claim-split", "", "pvc-77aa"),
	), missingClusterCounts{})

	assert.Equal(t, pvcInfoAttrs{volumeName: "pvc-9f3a"}, out[pvcKey{"c", "n", "claim-vol-only"}])
	assert.Equal(t, pvcInfoAttrs{storageClass: "gp3"}, out[pvcKey{"c", "n", "claim-sc-only"}])
	assert.Equal(t, pvcInfoAttrs{storageClass: "gp2", volumeName: "pvc-77aa"},
		out[pvcKey{"c", "n", "claim-split"}], "fields merge across sibling series")
}

// TestResolvePVCInfo_PerFieldDeterministicCollision — on duplicate
// (cluster, namespace, claim) each field independently resolves to the
// lexically-smallest non-empty value, regardless of vector order.
func TestResolvePVCInfo_PerFieldDeterministicCollision(t *testing.T) {
	t.Parallel()
	a := pvcInfoSample("c", "n", "claim", "gp3", "pvc-bbb")
	b := pvcInfoSample("c", "n", "claim", "gp2", "pvc-aaa")

	fwd := resolvePVCInfo(sampleVec(a, b), missingClusterCounts{})
	rev := resolvePVCInfo(sampleVec(b, a), missingClusterCounts{})
	want := pvcInfoAttrs{storageClass: "gp2", volumeName: "pvc-aaa"}
	assert.Equal(t, want, fwd[pvcKey{"c", "n", "claim"}])
	assert.Equal(t, want, rev[pvcKey{"c", "n", "claim"}], "order-independent")
}

// TestResolvePVCInfo_EmptyClaimSkipped — a series with no claim name carries
// no join key and is dropped.
func TestResolvePVCInfo_EmptyClaimSkipped(t *testing.T) {
	t.Parallel()
	out := resolvePVCInfo(sampleVec(pvcInfoSample("c", "n", "", "gp3", "pvc-x")), missingClusterCounts{})
	assert.Empty(t, out)
}

// --- Trident resolvers -----------------------------------------------------

// TestResolveTridentVolumeBackends — (cluster, name) → backendUUID with empty
// skips and lexical-min on duplicates; a missing cluster label buckets under
// "unknown".
func TestResolveTridentVolumeBackends(t *testing.T) {
	t.Parallel()
	mc := missingClusterCounts{}
	out := resolveTridentVolumeBackends(sampleVec(
		tridentVolumeSample("c-a", "pvc-9f3a", "be-1234"),
		tridentVolumeSample("c-a", "", "be-nokey"),  // empty name skipped
		tridentVolumeSample("c-a", "pvc-noval", ""), // empty backendUUID skipped
		// duplicate: lexically-smallest backendUUID wins
		tridentVolumeSample("c-a", "pvc-dup", "be-b"),
		tridentVolumeSample("c-a", "pvc-dup", "be-a"),
		// missing cluster → "unknown" bucket
		tridentVolumeSample("", "pvc-orphan", "be-orphan"),
	), mc)

	assert.Equal(t, map[[2]string]string{
		{"c-a", "pvc-9f3a"}:       "be-1234",
		{"c-a", "pvc-dup"}:        "be-a",
		{"unknown", "pvc-orphan"}: "be-orphan",
	}, out)
	assert.Equal(t, 1, mc[promql.QTridentVolumeInfo], "missing-cluster sample tallied")
}

// TestResolveTridentBackendSVMs — (cluster, backendUUID) → svm with empty
// skips and lexical-min on duplicates; a missing cluster label buckets under
// "unknown".
func TestResolveTridentBackendSVMs(t *testing.T) {
	t.Parallel()
	mc := missingClusterCounts{}
	out := resolveTridentBackendSVMs(sampleVec(
		tridentBackendSample("c-a", "be-1234", "svm-prod"),
		tridentBackendSample("c-a", "", "svm-nokey"), // empty backendUUID skipped
		tridentBackendSample("c-a", "be-noval", ""),  // empty svm skipped
		// duplicate: lexically-smallest svm wins
		tridentBackendSample("c-a", "be-dup", "svm-b"),
		tridentBackendSample("c-a", "be-dup", "svm-a"),
		// missing cluster → "unknown" bucket
		tridentBackendSample("", "be-orphan", "svm-orphan"),
	), mc)

	assert.Equal(t, map[[2]string]string{
		{"c-a", "be-1234"}:       "svm-prod",
		{"c-a", "be-dup"}:        "svm-a",
		{"unknown", "be-orphan"}: "svm-orphan",
	}, out)
	assert.Equal(t, 1, mc[promql.QTridentBackendInfo], "missing-cluster sample tallied")
}

// --- assembly (parseTopology) ----------------------------------------------

func pvcLabelsByID(t *testing.T, tp Topology, id string) map[string]string {
	t.Helper()
	for _, p := range tp.PVCs {
		if p.ID() == id {
			return p.Labels()
		}
	}
	t.Fatalf("PVC %q not found", id)
	return nil
}

// TestParseTopology_TridentFullChain — kube_persistentvolumeclaim_info's
// volumename chains through kube_tridentvolume_info (name → backendUUID) and
// kube_tridentbackend_info (backendUUID → svm) onto the PVC labels.
func TestParseTopology_TridentFullChain(t *testing.T) {
	tp := parseTopology(topologyVectors{
		PVC:            sampleVec(pvcBindingSample("c-a", "db", "mongo-0", "data-mongo-0")),
		PVCInfo:        sampleVec(pvcInfoSample("c-a", "db", "data-mongo-0", "netapp-nas", "pvc-9f3a")),
		TridentVolume:  sampleVec(tridentVolumeSample("c-a", "pvc-9f3a", "be-1234")),
		TridentBackend: sampleVec(tridentBackendSample("c-a", "be-1234", "svm-prod")),
	})

	labels := pvcLabelsByID(t, tp, "c-a/db/data-mongo-0")
	assert.Equal(t, "pvc-9f3a", labels["volumename"])
	assert.Equal(t, "svm-prod", labels["svm"])
}

// TestParseTopology_TridentPartialChains — every broken link omits exactly the
// downstream key(s): no TridentVolume row → volumename only; TridentVolume
// without a matching backend → volumename only; no volumename → neither key.
// Absent Trident metrics entirely → volumename still resolves, no svm, build OK.
func TestParseTopology_TridentPartialChains(t *testing.T) {
	pvcVec := sampleVec(
		pvcBindingSample("c-a", "db", "p1", "claim-full"),
		pvcBindingSample("c-a", "db", "p2", "claim-no-tv"),
		pvcBindingSample("c-a", "db", "p3", "claim-no-tb"),
		pvcBindingSample("c-a", "db", "p4", "claim-no-vol"),
	)
	infoVec := sampleVec(
		pvcInfoSample("c-a", "db", "claim-full", "", "pv-full"),
		pvcInfoSample("c-a", "db", "claim-no-tv", "", "pv-no-tv"),
		pvcInfoSample("c-a", "db", "claim-no-tb", "", "pv-no-tb"),
		pvcInfoSample("c-a", "db", "claim-no-vol", "gp3", ""), // storageclass only
	)
	tp := parseTopology(topologyVectors{
		PVC:     pvcVec,
		PVCInfo: infoVec,
		TridentVolume: sampleVec(
			tridentVolumeSample("c-a", "pv-full", "be-ok"),
			tridentVolumeSample("c-a", "pv-no-tb", "be-missing"),
		),
		TridentBackend: sampleVec(tridentBackendSample("c-a", "be-ok", "svm-1")),
	})

	full := pvcLabelsByID(t, tp, "c-a/db/claim-full")
	assert.Equal(t, "pv-full", full["volumename"])
	assert.Equal(t, "svm-1", full["svm"])

	noTV := pvcLabelsByID(t, tp, "c-a/db/claim-no-tv")
	assert.Equal(t, "pv-no-tv", noTV["volumename"])
	_, hasSVM := noTV["svm"]
	assert.False(t, hasSVM, "PV without a TridentVolume row must omit svm")

	noTB := pvcLabelsByID(t, tp, "c-a/db/claim-no-tb")
	assert.Equal(t, "pv-no-tb", noTB["volumename"])
	_, hasSVM = noTB["svm"]
	assert.False(t, hasSVM, "backendUUID without a TridentBackend row must omit svm")

	noVol := pvcLabelsByID(t, tp, "c-a/db/claim-no-vol")
	_, hasVolumename := noVol["volumename"]
	_, hasSVM = noVol["svm"]
	assert.False(t, hasVolumename, "no volumename label on the info series → key absent, never empty")
	assert.False(t, hasSVM, "svm is impossible without volumename")

	// Trident metrics absent entirely: volumename still resolves, no svm.
	tp2 := parseTopology(topologyVectors{PVC: pvcVec, PVCInfo: infoVec})
	require.Len(t, tp2.PVCs, 4)
	absent := pvcLabelsByID(t, tp2, "c-a/db/claim-full")
	assert.Equal(t, "pv-full", absent["volumename"])
	_, hasSVM = absent["svm"]
	assert.False(t, hasSVM, "no Trident metrics → no svm label, build still succeeds")
}

// TestParseTopology_VolumeAndVolumenameCoexist — the pod-spec `volume` key
// (from the binding metric) and the bound-PV `volumename` key (from
// kube_persistentvolumeclaim_info) are distinct labels on one PVC.
func TestParseTopology_VolumeAndVolumenameCoexist(t *testing.T) {
	binding := model.Sample{Metric: model.Metric{
		"cluster": "c-a", "namespace": "db", "pod": "mongo-0",
		"persistentvolumeclaim": "data-mongo-0", "volume": "data",
	}}
	tp := parseTopology(topologyVectors{
		PVC:     sampleVec(binding),
		PVCInfo: sampleVec(pvcInfoSample("c-a", "db", "data-mongo-0", "", "pvc-9f3a")),
	})

	labels := pvcLabelsByID(t, tp, "c-a/db/data-mongo-0")
	assert.Equal(t, "data", labels["volume"], "pod-spec volume name from the binding metric")
	assert.Equal(t, "pvc-9f3a", labels["volumename"], "bound PV name from kube_persistentvolumeclaim_info")
}

// TestParseTopology_TridentChainIsPerCluster — the joins never cross clusters:
// a TridentVolume/TridentBackend pair in cluster c-b does not resolve an svm
// for the same PV name in cluster c-a.
func TestParseTopology_TridentChainIsPerCluster(t *testing.T) {
	tp := parseTopology(topologyVectors{
		PVC:            sampleVec(pvcBindingSample("c-a", "db", "p", "claim")),
		PVCInfo:        sampleVec(pvcInfoSample("c-a", "db", "claim", "", "pvc-9f3a")),
		TridentVolume:  sampleVec(tridentVolumeSample("c-b", "pvc-9f3a", "be-1")),
		TridentBackend: sampleVec(tridentBackendSample("c-b", "be-1", "svm-other")),
	})

	labels := pvcLabelsByID(t, tp, "c-a/db/claim")
	assert.Equal(t, "pvc-9f3a", labels["volumename"])
	_, hasSVM := labels["svm"]
	assert.False(t, hasSVM, "Trident rows in another cluster must not resolve this PVC's svm")
}

// TestParseTopology_TridentUnknownBucketChain — a fully-unlabelled deployment
// (no `cluster` label on the binding, the PVC info, or either Trident series —
// the supported single-cluster case) buckets every series under
// cluster="unknown" consistently, so the whole chain still resolves: the
// unknown/<ns>/<claim> PVC carries BOTH volumename and svm. Guards the
// four independently-bucketed maps (binding assembly, resolvePVCInfo,
// resolveTridentVolumeBackends, resolveTridentBackendSVMs) agreeing on the
// "unknown" join key — a skip-guard or raw-label read in any one of them
// would silently drop the labels here.
func TestParseTopology_TridentUnknownBucketChain(t *testing.T) {
	tp := parseTopology(topologyVectors{
		PVC:            sampleVec(pvcBindingSample("", "db", "mongo-0", "data-mongo-0")),
		PVCInfo:        sampleVec(pvcInfoSample("", "db", "data-mongo-0", "netapp-nas", "pvc-9f3a")),
		TridentVolume:  sampleVec(tridentVolumeSample("", "pvc-9f3a", "be-1234")),
		TridentBackend: sampleVec(tridentBackendSample("", "be-1234", "svm-prod")),
	})

	labels := pvcLabelsByID(t, tp, "unknown/db/data-mongo-0")
	assert.Equal(t, "unknown", labels["cluster"])
	assert.Equal(t, "pvc-9f3a", labels["volumename"], "unlabelled PVC info still resolves volumename via the unknown bucket")
	assert.Equal(t, "svm-prod", labels["svm"], "unlabelled Trident rows still resolve svm via the unknown bucket")
}

// TestParseTopology_TridentBucketMismatchNoFallback — the per-cluster join is
// strict ACROSS buckets too: a labelled PVC must not resolve svm from
// unknown-bucketed Trident rows, and an unknown-bucketed PVC must not resolve
// svm from another cluster's labelled Trident rows. Guards against a future
// unknown-bucket fallback lookup in the assembly join, which would misattribute
// an arbitrary cluster's backend to another cluster's PVC.
func TestParseTopology_TridentBucketMismatchNoFallback(t *testing.T) {
	// Labelled PVC chain vs unlabelled ("unknown"-bucketed) Trident rows.
	labelled := parseTopology(topologyVectors{
		PVC:            sampleVec(pvcBindingSample("c-a", "db", "p", "claim")),
		PVCInfo:        sampleVec(pvcInfoSample("c-a", "db", "claim", "", "pvc-9f3a")),
		TridentVolume:  sampleVec(tridentVolumeSample("", "pvc-9f3a", "be-1")),
		TridentBackend: sampleVec(tridentBackendSample("", "be-1", "svm-x")),
	})
	got := pvcLabelsByID(t, labelled, "c-a/db/claim")
	assert.Equal(t, "pvc-9f3a", got["volumename"])
	_, hasSVM := got["svm"]
	assert.False(t, hasSVM, "labelled PVC must not fall back to unknown-bucketed Trident rows")

	// Unlabelled ("unknown"-bucketed) PVC chain vs labelled Trident rows.
	unknown := parseTopology(topologyVectors{
		PVC:            sampleVec(pvcBindingSample("", "db", "p", "claim")),
		PVCInfo:        sampleVec(pvcInfoSample("", "db", "claim", "", "pvc-9f3a")),
		TridentVolume:  sampleVec(tridentVolumeSample("c-a", "pvc-9f3a", "be-1")),
		TridentBackend: sampleVec(tridentBackendSample("c-a", "be-1", "svm-x")),
	})
	got = pvcLabelsByID(t, unknown, "unknown/db/claim")
	assert.Equal(t, "pvc-9f3a", got["volumename"])
	_, hasSVM = got["svm"]
	assert.False(t, hasSVM, "unknown-bucketed PVC must not resolve svm from a labelled cluster's Trident rows")
}

// TestParseTopology_TridentDeterministicAcrossOrder — shuffled Trident vectors
// resolve to the identical label set (lexical-min at each chain stage).
func TestParseTopology_TridentDeterministicAcrossOrder(t *testing.T) {
	pvcVec := sampleVec(pvcBindingSample("c", "n", "p", "claim"))
	infoVec := sampleVec(pvcInfoSample("c", "n", "claim", "", "pv-1"))
	tvA := tridentVolumeSample("c", "pv-1", "be-a")
	tvB := tridentVolumeSample("c", "pv-1", "be-b")
	tbA := tridentBackendSample("c", "be-a", "svm-a")
	tbB := tridentBackendSample("c", "be-a", "svm-b")

	fwd := parseTopology(topologyVectors{PVC: pvcVec, PVCInfo: infoVec,
		TridentVolume: sampleVec(tvA, tvB), TridentBackend: sampleVec(tbA, tbB)})
	rev := parseTopology(topologyVectors{PVC: pvcVec, PVCInfo: infoVec,
		TridentVolume: sampleVec(tvB, tvA), TridentBackend: sampleVec(tbB, tbA)})

	fwdLabels := pvcLabelsByID(t, fwd, "c/n/claim")
	revLabels := pvcLabelsByID(t, rev, "c/n/claim")
	assert.Equal(t, "svm-a", fwdLabels["svm"], "lexical-min at each stage: be-a then svm-a")
	assert.Equal(t, fwdLabels, revLabels, "byte-identical labels regardless of vector order")
}

// TestParseTopology_TridentNeverMaterialisesPVC — Trident series for a PV that
// no binding-derived PVC references must not create nodes.
func TestParseTopology_TridentNeverMaterialisesPVC(t *testing.T) {
	tp := parseTopology(topologyVectors{
		PVCInfo:        sampleVec(pvcInfoSample("c", "n", "claim-unmounted", "", "pv-x")),
		TridentVolume:  sampleVec(tridentVolumeSample("c", "pv-x", "be-1")),
		TridentBackend: sampleVec(tridentBackendSample("c", "be-1", "svm-1")),
	})
	assert.Empty(t, tp.PVCs, "info/Trident series enrich existing PVCs only — never materialise one")
}
