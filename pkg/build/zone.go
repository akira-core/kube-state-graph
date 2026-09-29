package build

import (
	"slices"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// zone is one `az` / `env` pair. An alert, a Kubernetes identity, an ONTAP
// cluster, a claim and a Harvest series each have a zone only when BOTH
// configured labels are non-empty — the same rule the identity ladder composes
// by.
type zone struct{ az, env string }

// zoneOf reads the pair through the configured label keys; ok is false when
// either is empty.
func zoneOf(m model.Metric, keys promql.LabelKeys) (zone, bool) {
	az, env := string(m[model.LabelName(keys.AZ)]), string(m[model.LabelName(keys.Env)])
	if az == "" || env == "" {
		return zone{}, false
	}
	return zone{az: az, env: env}, true
}

// zoneAdmits is the zone-agreement rule (read-storage-roots-through-volume-hub
// D11): a candidate is excluded only when BOTH sides know their zone and they
// disagree. An alert with no pair, or a candidate whose zone is unknown,
// falls back to the label comparison alone.
func zoneAdmits(candidate []zone, z zone, zoned bool) bool {
	return !zoned || len(candidate) == 0 || slices.Contains(candidate, z)
}

// zonesAgree is the same rule for two single-zone sides: they disagree only when
// BOTH carry a complete pair and the pairs differ. The claim → FlexVol join
// (accept-multi-zone-storage-graph) applies it to a claim's own zone and each
// matched volume_labels series'.
func zonesAgree(a zone, aKnown bool, b zone, bKnown bool) bool {
	return !aKnown || !bKnown || a == b
}

// less orders zones by (az, env), so a choice among several is deterministic.
func (z zone) less(o zone) bool {
	if z.az != o.az {
		return z.az < o.az
	}
	return z.env < o.env
}
