package build

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// TestWarnSelectorFamilyEmpty_NeverBlamesHarvest pins the routing-only
// contract on the Warn's side: the Harvest family renders no request matcher
// (az selects a backend, env is inert), so an empty volume_labels under an
// az- or env-scoped request can never be the request's doing and must not be
// named — while the kubelet family, which DOES carry the matcher, still is.
func TestWarnSelectorFamilyEmpty_NeverBlamesHarvest(t *testing.T) {
	// KSM matched, every other family issued and empty. tallySeries reports an
	// issued family that matched nothing as a present 0.
	raw := map[string]int{
		string(promql.QPodInfo):                    1,
		string(promql.QKubeletVolumeUsedBytes):     0,
		string(promql.QKubeletVolumeCapacityBytes): 0,
		string(promql.QVolumeLabels):               0,
	}

	for name, sel := range map[string]promql.Selector{
		"az":  {AZ: []string{"zone-a"}},
		"env": {Env: []string{"prod"}},
	} {
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)
			warnSelectorFamilyEmpty(t.Context(), sel, promql.LabelKeys{}, raw)

			out := buf.String()
			assert.Contains(t, out, "selector_family_empty", "the kubelet family is reached and empty")
			assert.Contains(t, out, string(promql.QKubeletVolumeUsedBytes))
			assert.NotContains(t, out, string(promql.QVolumeLabels),
				"Harvest carries no matcher under %s, so its emptiness is not the request's doing", name)
		})
	}
}

// A family absent from the tally was never issued — a hub-mode storage build
// whose claim scope came out empty reads no kubelet family at all — so its
// emptiness says nothing about the request's labels.
func TestWarnSelectorFamilyEmpty_NeverBlamesAnUnissuedFamily(t *testing.T) {
	raw := map[string]int{string(promql.QPodInfo): 1}
	buf := captureLogs(t)
	warnSelectorFamilyEmpty(t.Context(), promql.Selector{Namespace: []string{"shop"}}, promql.LabelKeys{}, raw)
	assert.Empty(t, buf.String())
}

// TestWarnSelectorFamilyEmpty_NeverBlamesAlerts pins the OTHER exclusion, on a
// different axis from Harvest's. ALERTS does carry az / env / namespace, so
// Selector.Reaches is true for exactly the dimensions this Warn tests — the
// matcher argument that clears Harvest does not clear alerts. What clears it is
// that an empty alert vector is the HEALTHY estate, the outcome the operator
// wants, and that the family is also the one a routing table may legitimately
// leave unserved. Either way the Warn would fire on a well-configured, quiet
// deployment on every filtered request.
func TestWarnSelectorFamilyEmpty_NeverBlamesAlerts(t *testing.T) {
	// Every kubelet leg populated, so the ONLY empty family left is ALERTS.
	raw := map[string]int{
		string(promql.QPodInfo):                    1,
		string(promql.QKubeletVolumeUsedBytes):     1,
		string(promql.QKubeletVolumeCapacityBytes): 1,
		string(promql.QVolumeLabels):               1,
	}

	for name, sel := range map[string]promql.Selector{
		"az":        {AZ: []string{"zone-a"}},
		"env":       {Env: []string{"prod"}},
		"namespace": {Namespace: []string{"shop"}},
	} {
		t.Run(name, func(t *testing.T) {
			// The premise: this dimension DOES reach ALERTS, so only the
			// explicit family exclusion can keep the Warn quiet.
			require.True(t, sel.Reaches(promql.QAlerts),
				"%s reaches ALERTS, so the exclusion cannot be an accident of Reaches", name)

			buf := captureLogs(t)
			warnSelectorFamilyEmpty(t.Context(), sel, promql.LabelKeys{}, raw)

			assert.Empty(t, buf.String(),
				"an empty alert vector under %s is the healthy estate, not a labelling mistake", name)
		})
	}
}
