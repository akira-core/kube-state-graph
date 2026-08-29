package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The routing-file rules themselves are pinned in pkg/promql/backendsfile and
// pkg/promql, where they now live. What is left to prove here is that the
// wrappers this package keeps for the binary's call sites delegate to that code
// rather than to a second, drifting copy of it.

// env builds a LookupEnvFunc over a fixed map, so a credential test never
// touches the real process environment.
func env(vars map[string]string) LookupEnvFunc {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func noEnv() LookupEnvFunc { return env(nil) }

const yamlTable = `
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-a]
  - name: netapp-a
    url: http://vm-netapp:8428
    families: [harvest]
    zones: [zone-a]
`

func TestParseBackendsFile_DelegatesToBackendsFile(t *testing.T) {
	tbl, err := ParseBackendsFile([]byte(yamlTable), noEnv())
	require.NoError(t, err)
	require.Equal(t, 2, tbl.Len())
	assert.Equal(t, "netapp-a", tbl.Backends()[0].Name())
}

// The credential rules reach the wrapper unchanged: a named-but-unset variable
// is a load failure, not a quiet fallback.
func TestParseBackendsFile_DelegatesCredentialRules(t *testing.T) {
	_, err := ParseBackendsFile([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
    usernameEnv: KSG_PROM_USERNAME_A
    passwordEnv: KSG_PROM_PASSWORD_A
`), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unset or empty")
}

func TestReadBackendsFile_DelegatesToBackendsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlTable), 0o600))

	tbl, err := ReadBackendsFile(path, noEnv())
	require.NoError(t, err)
	assert.Equal(t, 2, tbl.Len())
}

func TestReadBackendsFile_MissingFile(t *testing.T) {
	_, err := ReadBackendsFile(filepath.Join(t.TempDir(), "absent.yaml"), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.yaml")
}

// A nil lookup must survive the conversion to the backendsfile function type
// and still mean "read the process environment".
func TestParseBackendsFile_NilLookupReadsTheEnvironment(t *testing.T) {
	t.Setenv("KSG_PROM_USERNAME", "from-env")
	t.Setenv("KSG_PROM_PASSWORD", "from-env-pw")

	tbl, err := ParseBackendsFile([]byte(yamlTable), nil)
	require.NoError(t, err)
	u, p := tbl.Backends()[0].Credentials()
	assert.Equal(t, "from-env", u)
	assert.Equal(t, "from-env-pw", p)
}

func TestSingleBackendTable_DelegatesToPromQL(t *testing.T) {
	tbl, err := SingleBackendTable("http://vm.example:8428", "u", "p")
	require.NoError(t, err)
	require.Equal(t, 1, tbl.Len())

	b := tbl.Backends()[0]
	assert.Equal(t, DefaultBackendName, b.Name())
	assert.Equal(t, promql.DefaultBackendName, DefaultBackendName)
	assert.Equal(t, "http://vm.example:8428", b.URL())
	assert.Len(t, b.Families(), 5, "the implicit backend serves every family")
	assert.Empty(t, b.Zones(), "the implicit backend is a catch-all")

	u, p := b.Credentials()
	assert.Equal(t, "u", u)
	assert.Equal(t, "p", p)
}
