package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
)

const argvSecret = "s3cret-argv-value"

type logLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func parseLog(t *testing.T, buf *bytes.Buffer) []logLine {
	t.Helper()
	var out []logLine
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var l logLine
		require.NoError(t, dec.Decode(&l))
		out = append(out, l)
	}
	return out
}

func warnsContaining(lines []logLine, substr string) int {
	n := 0
	for _, l := range lines {
		if l.Level == "WARN" && strings.Contains(l.Msg, substr) {
			n++
		}
	}
	return n
}

func anyContaining(lines []logLine, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l.Msg, substr) {
			return true
		}
	}
	return false
}

func writeKeys(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// A configured key source that yields zero keys leaves auth OFF, so it must be
// reported as disabled — never as "auth enabled".
func TestLoadAPIKeys_ZeroKeysWarnsDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func() config.Config
	}{
		{"empty file", func() config.Config {
			c := config.Defaults()
			c.APIKeysFile = writeKeys(t, "")
			c.APIKeysReloadInterval = 0
			return c
		}},
		{"comment-only file", func() config.Config {
			c := config.Defaults()
			c.APIKeysFile = writeKeys(t, "# rotated out\n\n   \n")
			c.APIKeysReloadInterval = 0
			return c
		}},
		{"separator-only key list", func() config.Config {
			c := config.Defaults()
			c.APIKeys = " , ,"
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ks, err := loadAPIKeys(t.Context(), tc.cfg(), capturingLogger(&buf))
			require.NoError(t, err)
			require.True(t, ks.Empty(), "auth is in fact disabled")

			lines := parseLog(t, &buf)
			assert.Equal(t, 1, warnsContaining(lines, "api key auth DISABLED"), "one Warn says auth is disabled")
			assert.False(t, anyContaining(lines, "auth enabled"), "no line may claim auth is enabled")
		})
	}
}

// A populated key file keeps its Info line and gets no disabled Warn.
func TestLoadAPIKeys_PopulatedFileIsEnabled(t *testing.T) {
	cfg := config.Defaults()
	cfg.APIKeysFile = writeKeys(t, argvSecret+"\n")
	cfg.APIKeysReloadInterval = 0

	var buf bytes.Buffer
	ks, err := loadAPIKeys(t.Context(), cfg, capturingLogger(&buf))
	require.NoError(t, err)
	require.False(t, ks.Empty())

	lines := parseLog(t, &buf)
	assert.True(t, anyContaining(lines, "api key auth enabled (file)"))
	assert.Zero(t, warnsContaining(lines, "DISABLED"))
	assert.NotContains(t, buf.String(), argvSecret, "a key value never reaches the log")
}

func TestWarnArgvSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		env        map[string]string
		wantAPIKey int
		wantDSN    int
	}{
		{name: "literal --api-keys flag", args: []string{"--api-keys=" + argvSecret}, wantAPIKey: 1},
		{name: "--api-keys beside --api-keys-file", args: []string{"--api-keys=" + argvSecret, "--api-keys-file=/keys"}, wantAPIKey: 1},
		{name: "keys from KSG_API_KEYS", env: map[string]string{"KSG_API_KEYS": argvSecret}},
		{name: "DSN with password", args: []string{"--route-store-dsn=clickhouse://ksg:" + argvSecret + "@ch:9000/routing"}, wantDSN: 1},
		{name: "DSN with user only", args: []string{"--route-store-dsn=clickhouse://ksg@ch:9000/routing"}, wantDSN: 1},
		{
			name:    "DSN with password from env, env credentials set",
			env:     map[string]string{"KSG_ROUTE_STORE_DSN": "clickhouse://ksg:" + argvSecret + "@ch:9000/routing", "KSG_ROUTE_STORE_USERNAME": "u", "KSG_ROUTE_STORE_PASSWORD": "p"},
			wantDSN: 1,
		},
		{name: "DSN without userinfo", args: []string{"--route-store-dsn=clickhouse://ch:9000/routing"}},
		{name: "unparseable DSN", args: []string{"--route-store-dsn=clickhouse://ksg:" + argvSecret + "@ch:90x0/routing"}},
		{name: "nothing configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse(tc.args, func(k string) (string, bool) {
				v, ok := tc.env[k]
				return v, ok
			})
			require.NoError(t, err)

			var buf bytes.Buffer
			warnArgvSecrets(cfg, capturingLogger(&buf))
			lines := parseLog(t, &buf)

			assert.Equal(t, tc.wantAPIKey, warnsContaining(lines, "--api-keys flag"))
			assert.Equal(t, tc.wantDSN, warnsContaining(lines, "route-store DSN embeds credentials"))
			assert.NotContains(t, buf.String(), argvSecret, "no Warn carries the secret")
			assert.NotContains(t, buf.String(), "ch:90x0", "no Warn carries the DSN or its parse error")
		})
	}
}
