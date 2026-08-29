package build

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// TestWarnSelectorFamilyEmpty_NeverBlamesHarvest pins the routing-only
// contract on the Warn's side: the Harvest family renders no request matcher
// (az selects a backend, env is inert), so an empty volume_labels under an
// az- or env-scoped request can never be the request's doing and must not be
// named — while the kubelet family, which DOES carry the matcher, still is.
func TestWarnSelectorFamilyEmpty_NeverBlamesHarvest(t *testing.T) {
	raw := map[string]int{string(promql.QPodInfo): 1} // KSM matched, every other family empty

	for name, sel := range map[string]promql.Selector{
		"az":  {AZ: []string{"zone-a"}},
		"env": {Env: []string{"prod"}},
	} {
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)
			warnSelectorFamilyEmpty(context.Background(), sel, promql.LabelKeys{}, raw)

			out := buf.String()
			assert.Contains(t, out, "selector_family_empty", "the kubelet family is reached and empty")
			assert.Contains(t, out, string(promql.QKubeletVolumeUsedBytes))
			assert.NotContains(t, out, string(promql.QVolumeLabels),
				"Harvest carries no matcher under %s, so its emptiness is not the request's doing", name)
		})
	}
}
