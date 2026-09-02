package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

func TestParse_DefaultsValid(t *testing.T) {
	cfg, err := Parse(nil, func(string) (string, bool) { return "", false })
	require.NoError(t, err, "defaults rejected")
	assert.NotZero(t, cfg.BuildTimeout, "expected non-zero BuildTimeout default")
	assert.NotZero(t, cfg.APITimeout, "expected non-zero APITimeout default")
}

func TestParse_FlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"KSG_PROM_URL":      "http://env:9090",
		"KSG_LISTEN_ADDR":   ":1111",
		"KSG_BUILD_TIMEOUT": "5s",
		"KSG_API_TIMEOUT":   "2s",
	}
	cfg, err := Parse(
		[]string{"--listen-addr=:2222", "--api-timeout=3s"},
		func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	)
	require.NoError(t, err)
	assert.Equal(t, "http://env:9090", cfg.PromURL, "env not honoured")
	assert.Equal(t, ":2222", cfg.ListenAddr, "flag did not override env")
	assert.Equal(t, 3*time.Second, cfg.APITimeout, "flag did not override env for api-timeout")
	assert.Equal(t, 5*time.Second, cfg.BuildTimeout, "build-timeout env not honoured")
}

func TestValidate_RejectsZeroAPITimeout(t *testing.T) {
	cfg := Defaults()
	cfg.APITimeout = 0
	assert.Error(t, cfg.Validate(), "expected error for zero api-timeout")
}

// F22: a negative reload interval silently disabled hot reload; it must now be
// rejected (0 stays the documented disable sentinel).
func TestValidate_RejectsNegativeReloadInterval(t *testing.T) {
	cfg := Defaults()
	cfg.APIKeysReloadInterval = -time.Second
	require.Error(t, cfg.Validate(), "expected error for negative api-keys-reload-interval")

	cfg.APIKeysReloadInterval = 0
	assert.NoError(t, cfg.Validate(), "zero must remain valid (disables hot reload)")
}

// F4: an invalid duration env var must fail loudly instead of silently keeping
// the default — parity with the flag path.
func TestParse_RejectsInvalidDurationEnv(t *testing.T) {
	for _, env := range []string{"KSG_BUILD_TIMEOUT", "KSG_API_TIMEOUT", "KSG_API_KEYS_RELOAD_INTERVAL"} {
		t.Run(env, func(t *testing.T) {
			_, err := Parse(nil, func(k string) (string, bool) {
				if k == env {
					return "15", true // missing unit → ParseDuration error
				}
				return "", false
			})
			require.Error(t, err, "invalid %s must fail parsing", env)
			assert.Contains(t, err.Error(), env, "error should name the offending env var")
		})
	}
}

func TestValidate_RejectsInvalidPromURL(t *testing.T) {
	cfg := Defaults()
	cfg.PromURL = "not-a-url"
	assert.Error(t, cfg.Validate(), "expected error for invalid prom-url")
}

func TestValidate_RejectsBadLogLevel(t *testing.T) {
	cfg := Defaults()
	cfg.LogLevel = "trace"
	assert.Error(t, cfg.Validate(), "expected error for invalid log-level")
}

func TestSplitAndTrim(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, splitAndTrim(" a, b ,, c "))
}

// Upstream basic-auth credentials are env-only (D-A1) and must be configured
// as a pair (D-A2); validation errors must never echo the configured values
// (D-A5).
func TestParse_PromBasicAuth_EnvPair(t *testing.T) {
	env := map[string]string{
		"KSG_PROM_USERNAME": "ksg",
		"KSG_PROM_PASSWORD": "s3cret",
	}
	cfg, err := Parse(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	assert.Equal(t, "ksg", cfg.PromUsername)
	assert.Equal(t, "s3cret", cfg.PromPassword)
}

func TestParse_PromBasicAuth_DefaultUnset(t *testing.T) {
	cfg, err := Parse(nil, func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	assert.Empty(t, cfg.PromUsername)
	assert.Empty(t, cfg.PromPassword)
}

func TestValidate_PromBasicAuth_HalfConfiguredRejected(t *testing.T) {
	cases := map[string]Config{
		"username only": func() Config { c := Defaults(); c.PromUsername = "ksg"; return c }(),
		"password only": func() Config { c := Defaults(); c.PromPassword = "s3cret-value"; return c }(),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			err := cfg.Validate()
			require.Error(t, err, "half-configured credentials must be rejected")
			assert.Contains(t, err.Error(), "KSG_PROM_USERNAME", "error should name both env vars")
			assert.Contains(t, err.Error(), "KSG_PROM_PASSWORD", "error should name both env vars")
			assert.NotContains(t, err.Error(), "ksg", "error must not echo the username")
			assert.NotContains(t, err.Error(), "s3cret-value", "error must not echo the password")
		})
	}
}

// RFC 7617 forbids ':' in basic-auth user-ids — SetBasicAuth would silently
// shift everything after the colon into the password, turning a typo into a
// permanent upstream 401. Validation fails fast without echoing the value.
func TestValidate_PromBasicAuth_ColonInUsernameRejected(t *testing.T) {
	cfg := Defaults()
	cfg.PromUsername = "team:reader"
	cfg.PromPassword = "s3cret-value"

	err := cfg.Validate()
	require.Error(t, err, "username containing ':' must be rejected")
	assert.Contains(t, err.Error(), "KSG_PROM_USERNAME")
	assert.NotContains(t, err.Error(), "team:reader", "error must not echo the username")
	assert.NotContains(t, err.Error(), "s3cret-value", "error must not echo the password")
}

// Credentials are deliberately env-only: no --prom-username / --prom-password
// flags exist, so flag parsing must reject them as unknown (D-A1).
func TestParse_PromBasicAuth_NoFlagsRegistered(t *testing.T) {
	for _, arg := range []string{"--prom-username=x", "--prom-password=x"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Parse([]string{arg}, func(string) (string, bool) { return "", false })
			require.Error(t, err, "credential flags must not exist")
		})
	}
}

// Route-store credentials are env-only and must be configured as a pair;
// validation errors must never echo the configured values.
func TestParse_RouteStoreAuth_EnvPair(t *testing.T) {
	env := map[string]string{
		"KSG_ROUTE_STORE_USERNAME": "ksg",
		"KSG_ROUTE_STORE_PASSWORD": "s3cret",
	}
	cfg, err := Parse(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	assert.Equal(t, "ksg", cfg.RouteStoreUsername)
	assert.Equal(t, "s3cret", cfg.RouteStorePassword)
}

func TestParse_RouteStoreAuth_DefaultUnset(t *testing.T) {
	cfg, err := Parse(nil, func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	assert.Empty(t, cfg.RouteStoreUsername)
	assert.Empty(t, cfg.RouteStorePassword)
}

func TestValidate_RouteStoreAuth_HalfConfiguredRejected(t *testing.T) {
	cases := map[string]Config{
		"username only": func() Config { c := Defaults(); c.RouteStoreUsername = "ksg"; return c }(),
		"password only": func() Config { c := Defaults(); c.RouteStorePassword = "s3cret-value"; return c }(),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			err := cfg.Validate()
			require.Error(t, err, "half-configured credentials must be rejected")
			assert.Contains(t, err.Error(), "KSG_ROUTE_STORE_USERNAME", "error should name both env vars")
			assert.Contains(t, err.Error(), "KSG_ROUTE_STORE_PASSWORD", "error should name both env vars")
			assert.NotContains(t, err.Error(), "ksg", "error must not echo the username")
			assert.NotContains(t, err.Error(), "s3cret-value", "error must not echo the password")
		})
	}
}

// Credentials are deliberately env-only: no --route-store-username /
// --route-store-password flags exist.
func TestParse_RouteStoreAuth_NoFlagsRegistered(t *testing.T) {
	for _, arg := range []string{"--route-store-username=x", "--route-store-password=x"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Parse([]string{arg}, func(string) (string, bool) { return "", false })
			require.Error(t, err, "credential flags must not exist")
		})
	}
}

// The az / env label keys default to the built-in binding, are overridable by
// env and then by flag, and are validated as PromQL label names at startup —
// an invalid key would otherwise break every topology query at request time.
func TestParse_LabelKeys(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }

	cfg, err := Parse(nil, noEnv)
	require.NoError(t, err)
	assert.Equal(t, "az", cfg.AZLabel)
	assert.Equal(t, "env", cfg.EnvLabel)

	env := map[string]string{"KSG_AZ_LABEL": "topology_zone", "KSG_ENV_LABEL": "deployment_tier"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err = Parse(nil, lookup)
	require.NoError(t, err)
	assert.Equal(t, "topology_zone", cfg.AZLabel)
	assert.Equal(t, "deployment_tier", cfg.EnvLabel)

	cfg, err = Parse([]string{"--az-label=zone"}, lookup)
	require.NoError(t, err)
	assert.Equal(t, "zone", cfg.AZLabel, "flag overrides env")
	assert.Equal(t, "deployment_tier", cfg.EnvLabel)
}

func TestParse_LabelKeysRejected(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"dotted az key", []string{"--az-label=topology.kubernetes.io/zone"}, "KSG_AZ_LABEL"},
		{"empty env key", []string{"--env-label="}, "KSG_ENV_LABEL"},
		{"identical keys", []string{"--az-label=scope", "--env-label=scope"}, "must differ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args, noEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// --- backend routing configuration ---------------------------------------

func TestParse_BackendsDefaults(t *testing.T) {
	cfg := Defaults()
	assert.Empty(t, cfg.BackendsFile, "no routing file by default — the implicit single backend serves")
	assert.Equal(t, 30*time.Second, cfg.BackendsReloadInterval,
		"the routing table reloads on the same cadence as the API-key file")
	require.NoError(t, cfg.Validate())
}

func TestParse_BackendsFlagOverridesEnv(t *testing.T) {
	env := map[string]string{
		"KSG_BACKENDS_FILE":            "/from/env.yaml",
		"KSG_BACKENDS_RELOAD_INTERVAL": "45s",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	// Env alone.
	cfg, err := Parse(nil, lookup)
	require.NoError(t, err)
	assert.Equal(t, "/from/env.yaml", cfg.BackendsFile)
	assert.Equal(t, 45*time.Second, cfg.BackendsReloadInterval)

	// Flags win.
	cfg, err = Parse([]string{"--backends-file=/from/flag.yaml", "--backends-reload-interval=10s"}, lookup)
	require.NoError(t, err)
	assert.Equal(t, "/from/flag.yaml", cfg.BackendsFile)
	assert.Equal(t, 10*time.Second, cfg.BackendsReloadInterval)
}

func TestParse_RejectsInvalidBackendsReloadIntervalEnv(t *testing.T) {
	_, err := Parse(nil, func(k string) (string, bool) {
		if k == "KSG_BACKENDS_RELOAD_INTERVAL" {
			return "45", true // missing unit
		}
		return "", false
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KSG_BACKENDS_RELOAD_INTERVAL")
}

// Zero stays the documented disable sentinel; a negative value is rejected
// rather than silently disabling reloads.
func TestValidate_RejectsNegativeBackendsReloadInterval(t *testing.T) {
	cfg := Defaults()
	cfg.BackendsReloadInterval = -time.Second
	require.Error(t, cfg.Validate())

	cfg.BackendsReloadInterval = 0
	assert.NoError(t, cfg.Validate())
}

// The NetApp volume-key derivation defaults to the provisioner-agnostic
// `-` → `_` rewrite matched as a suffix, is overridable by env and then by
// flag, and is compiled at startup so an invalid pattern never reaches a build.
func TestParse_NetAppVolumeKey(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }

	cfg, err := Parse(nil, noEnv)
	require.NoError(t, err)
	assert.Nil(t, cfg.NetAppVolumeKeyRewrite,
		"nil means the operator configured none, which adopts the build defaults")
	assert.Equal(t, string(build.DefaultVolumeMatchMode), cfg.NetAppVolumeMatchMode)
	assert.Equal(t, build.DefaultQoSScopeBatchBytes, cfg.NetAppQoSScopeBatchBytes)

	env := map[string]string{
		"KSG_NETAPP_VOLUME_KEY_REWRITE":    `-=_ ; ^=vol_`,
		"KSG_NETAPP_VOLUME_MATCH_MODE":     "contains",
		"KSG_NETAPP_QOS_SCOPE_BATCH_BYTES": "4096",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err = Parse(nil, lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{"-=_", "^=vol_"}, cfg.NetAppVolumeKeyRewrite,
		"the env form splits on semicolons and trims")
	assert.Equal(t, "contains", cfg.NetAppVolumeMatchMode)
	assert.Equal(t, 4096, cfg.NetAppQoSScopeBatchBytes)

	cfg, err = Parse([]string{
		"--netapp-volume-key-rewrite=-=_",
		"--netapp-volume-match-mode=exact",
	}, lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{"-=_"}, cfg.NetAppVolumeKeyRewrite,
		"the flag replaces the env list wholesale rather than appending to it")
	assert.Equal(t, "exact", cfg.NetAppVolumeMatchMode, "flag overrides env")
	assert.Equal(t, 4096, cfg.NetAppQoSScopeBatchBytes, "untouched dimensions keep the env value")

	cfg, err = Parse([]string{
		"--netapp-volume-key-rewrite=-=_",
		"--netapp-volume-key-rewrite=^=trident_",
	}, noEnv)
	require.NoError(t, err)
	assert.Equal(t, []string{"-=_", "^=trident_"}, cfg.NetAppVolumeKeyRewrite,
		"repeated flags accumulate in declaration order")
}

func TestParse_NetAppVolumeKeyRejected(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"uncompilable pattern", []string{"--netapp-volume-key-rewrite=([=x"}, `"(["`},
		{"missing separator", []string{"--netapp-volume-key-rewrite=nosep"}, "no \"=\" separator"},
		{"empty pattern", []string{"--netapp-volume-key-rewrite==x"}, "empty pattern"},
		{"unknown match mode", []string{"--netapp-volume-match-mode=prefix"}, "prefix"},
		{"non-positive batch budget", []string{"--netapp-qos-scope-batch-bytes=0"}, "must be > 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.args, noEnv)
			require.Error(t, err, "an invalid derivation must not start with a default substituted")
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// The parse layer owns the `<pattern>=<replacement>` grammar; a replacement is
// free to contain `=` because only the FIRST one separates.
func TestParseVolumeKeyRules(t *testing.T) {
	rules, err := ParseVolumeKeyRules([]string{"-=_", `\x3d=EQ`, "a=b=c"})
	require.NoError(t, err)
	assert.Equal(t, []build.VolumeKeyRule{
		{Pattern: "-", Replacement: "_"},
		{Pattern: `\x3d`, Replacement: "EQ"},
		{Pattern: "a", Replacement: "b=c"},
	}, rules)

	nilRules, err := ParseVolumeKeyRules(nil)
	require.NoError(t, err)
	assert.Nil(t, nilRules, "nil stays nil so the build layer applies its defaults")

	empty, err := ParseVolumeKeyRules([]string{})
	require.NoError(t, err)
	assert.NotNil(t, empty, "an explicitly empty list is an identity rewrite, not the default")
	assert.Empty(t, empty)
}
