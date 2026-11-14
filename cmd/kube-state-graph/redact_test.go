package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
)

const promURLSecret = "s3cret-in-prom-url"

// --prom-url userinfo never reaches a startup log line, on either the
// implicit-backend path or the ignored-because-of-a-file path, and a
// routing-file url renders redacted in the table log.
func TestInitialTable_PromURLUserinfoIsRedacted(t *testing.T) {
	raw := "http://ksg:" + promURLSecret + "@vm.example:8428"

	t.Run("implicit backend", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.PromURL = raw
		var buf bytes.Buffer
		tbl, _, err := initialTable(cfg, capturingLogger(&buf), noEnv)
		require.NoError(t, err)
		assert.NotContains(t, buf.String(), promURLSecret)
		assert.Contains(t, buf.String(), "http://ksg:xxxxx@vm.example:8428")
		assert.Equal(t, raw, tbl.Backends()[0].URL(), "the dial target is unchanged")
	})

	t.Run("ignored beside a routing file", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.PromURL = raw
		cfg.BackendsFile = writeTable(t, twoBackends)
		var buf bytes.Buffer
		_, _, err := initialTable(cfg, capturingLogger(&buf), noEnv)
		require.NoError(t, err)
		assert.Contains(t, buf.String(), "--prom-url is ignored")
		assert.NotContains(t, buf.String(), promURLSecret)
	})

	t.Run("routing-file url", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.BackendsFile = writeTable(t, `
backends:
  - name: zone-a
    url: http://ksg:`+promURLSecret+`@vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
`)
		var buf bytes.Buffer
		_, _, err := initialTable(cfg, capturingLogger(&buf), noEnv)
		require.NoError(t, err)
		assert.Contains(t, buf.String(), "url=http://ksg:xxxxx@vm-a:8428")
		assert.NotContains(t, buf.String(), promURLSecret)
	})
}
