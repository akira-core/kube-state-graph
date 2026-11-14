package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestDefaults_AdmissionAndWindow(t *testing.T) {
	cfg, err := Parse(nil, noEnv())
	require.NoError(t, err)
	assert.Equal(t, 32, cfg.MaxInflightBuilds, "admission is on by default")
	assert.Equal(t, promql.DefaultMaxConcurrency, DefaultMaxInflightBuilds)
	assert.Zero(t, cfg.MaxWindow, "no window cap by default")
}

func TestParse_AdmissionAndWindowFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"KSG_MAX_INFLIGHT_BUILDS": "4",
		"KSG_MAX_WINDOW":          "24h",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	cfg, err := Parse(nil, lookup)
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.MaxInflightBuilds)
	assert.Equal(t, 24*time.Hour, cfg.MaxWindow)

	cfg, err = Parse([]string{"--max-inflight-builds=0", "--max-window=1h"}, lookup)
	require.NoError(t, err)
	assert.Zero(t, cfg.MaxInflightBuilds)
	assert.Equal(t, time.Hour, cfg.MaxWindow)
}

func TestParse_AdmissionAndWindowEnvMustParse(t *testing.T) {
	for env, val := range map[string]string{
		"KSG_MAX_INFLIGHT_BUILDS": "lots",
		"KSG_MAX_WINDOW":          "24",
	} {
		_, err := Parse(nil, func(k string) (string, bool) {
			if k == env {
				return val, true
			}
			return "", false
		})
		require.Error(t, err, env)
		assert.Contains(t, err.Error(), env)
	}
}

func TestValidate_AdmissionAndWindowRejections(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"negative inflight bound": func(c *Config) { c.MaxInflightBuilds = -1 },
		"negative window cap":     func(c *Config) { c.MaxWindow = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			mutate(&cfg)
			require.Error(t, cfg.Validate())
		})
	}
}
