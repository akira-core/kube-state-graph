package build

import (
	"strings"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/prometheus/common/model"
)

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
	// zone is the (az, env) pair of the claim's info series; the zero zone when
	// no series carried the complete pair. It is the claim's side of the
	// zone-agreeing FlexVol join.
	zone zone
}

// resolvePVCInfo builds the (cluster, namespace, persistentvolumeclaim) →
// {storageclass, volumename} index from kube_persistentvolumeclaim_info. The
// result enriches PVC nodes that already exist (from the pod→PVC binding
// metric); only a claim-seeded build (MaterialiseUnboundClaims) also
// materialises a root claim no pod mounts from its entry.
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
		if z, ok := zoneOf(s.Metric, mc.keys); ok && (!attrs.zone.known() || z.compare(attrs.zone) < 0) {
			attrs.zone = z
		}
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
