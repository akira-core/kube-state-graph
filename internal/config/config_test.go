package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestParse_MetricPrefix_DefaultEmpty(t *testing.T) {
	cfg, err := Parse(nil, func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	assert.Empty(t, cfg.MetricPrefix, "metric-prefix default should be empty")
}

func TestParse_MetricPrefix_EnvAndFlag(t *testing.T) {
	t.Run("env wins over default", func(t *testing.T) {
		cfg, err := Parse(nil, func(k string) (string, bool) {
			if k == "KSG_METRIC_PREFIX" {
				return "o11y_", true
			}
			return "", false
		})
		require.NoError(t, err)
		assert.Equal(t, "o11y_", cfg.MetricPrefix)
	})
	t.Run("flag wins over env", func(t *testing.T) {
		cfg, err := Parse(
			[]string{"--metric-prefix=beta_"},
			func(k string) (string, bool) {
				if k == "KSG_METRIC_PREFIX" {
					return "acme_", true
				}
				return "", false
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "beta_", cfg.MetricPrefix)
	})
}

func TestValidate_MetricPrefix(t *testing.T) {
	cases := map[string]struct {
		prefix  string
		wantErr bool
	}{
		"empty":             {prefix: "", wantErr: false},
		"underscore suffix": {prefix: "o11y_", wantErr: false},
		"colon allowed":     {prefix: "acme:tenant_", wantErr: false},
		"alpha only":        {prefix: "acme", wantErr: false},
		"hyphen rejected":   {prefix: "o11y-bad!", wantErr: true},
		"leading digit":     {prefix: "1starts_with_digit", wantErr: true},
		"trailing space":    {prefix: "o11y_ ", wantErr: true},
		"embedded space":    {prefix: "o 11y_", wantErr: true},
		"unicode rejected":  {prefix: "o11y✓_", wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.MetricPrefix = tc.prefix
			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err, "expected error for prefix %q", tc.prefix)
				assert.Contains(t, err.Error(), "metric-prefix", "error should mention metric-prefix")
			} else {
				require.NoError(t, err, "did not expect error for prefix %q", tc.prefix)
			}
		})
	}
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
