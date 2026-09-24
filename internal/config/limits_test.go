package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The server's defaults ARE the library's exported defaults, so an embedder
// writing no option runs with exactly what the server runs with.
func TestDefaults_LimitCacheAlignMatchLibrary(t *testing.T) {
	cfg, err := Parse(nil, noEnv())
	require.NoError(t, err)
	assert.Equal(t, promql.DefaultMaxConcurrency, cfg.UpstreamMaxConcurrency)
	assert.Equal(t, promql.DefaultQueryCacheMaxSeries, cfg.QueryCacheMaxSeries)
	assert.Equal(t, promql.DefaultQueryCacheTTL, cfg.QueryCacheTTL)
	assert.Equal(t, kubegraph.DefaultEndAlign, cfg.EndAlign)
}

func TestParse_LimitCacheAlignFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"KSG_UPSTREAM_MAX_CONCURRENCY": "8",
		"KSG_QUERY_CACHE_MAX_SERIES":   "500",
		"KSG_QUERY_CACHE_TTL":          "10s",
		"KSG_END_ALIGN":                "15s",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	cfg, err := Parse(nil, lookup)
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.UpstreamMaxConcurrency)
	assert.Equal(t, 500, cfg.QueryCacheMaxSeries)
	assert.Equal(t, 10*time.Second, cfg.QueryCacheTTL)
	assert.Equal(t, 15*time.Second, cfg.EndAlign)

	cfg, err = Parse([]string{
		"--upstream-max-concurrency=0", "--query-cache-max-series=0", "--end-align=0",
	}, lookup)
	require.NoError(t, err)
	assert.Zero(t, cfg.UpstreamMaxConcurrency)
	assert.Zero(t, cfg.QueryCacheMaxSeries)
	assert.Zero(t, cfg.EndAlign)
}

func TestValidate_LimitCacheAlignRejections(t *testing.T) {
	cases := map[string]func(*Config){
		"negative concurrency":    func(c *Config) { c.UpstreamMaxConcurrency = -1 },
		"negative cache budget":   func(c *Config) { c.QueryCacheMaxSeries = -1 },
		"zero ttl with cache on":  func(c *Config) { c.QueryCacheTTL = 0 },
		"negative ttl with cache": func(c *Config) { c.QueryCacheTTL = -time.Second },
		"negative end alignment":  func(c *Config) { c.EndAlign = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			mutate(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}

	cfg := Defaults()
	cfg.QueryCacheMaxSeries = 0
	cfg.QueryCacheTTL = 0
	assert.NoError(t, cfg.Validate(), "TTL is irrelevant once the cache is off")
}

func TestParse_RejectsInvalidLimitEnv(t *testing.T) {
	for env, val := range map[string]string{
		"KSG_UPSTREAM_MAX_CONCURRENCY": "lots",
		"KSG_QUERY_CACHE_MAX_SERIES":   "1e5",
		"KSG_QUERY_CACHE_TTL":          "60",
		"KSG_END_ALIGN":                "30",
	} {
		t.Run(env, func(t *testing.T) {
			_, err := Parse(nil, func(k string) (string, bool) {
				if k == env {
					return val, true
				}
				return "", false
			})
			assert.Error(t, err)
		})
	}
}
