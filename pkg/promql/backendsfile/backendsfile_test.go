package backendsfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

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

const jsonTable = `{
  "backends": [
    {"name": "zone-a", "url": "http://vm-a:8428", "families": ["ksm","kubelet","servicegraph","probe"], "zones": ["zone-a"]},
    {"name": "netapp-a", "url": "http://vm-netapp:8428", "families": ["harvest"], "zones": ["zone-a"]}
  ]
}`

// The routing table lives in a ConfigMap, which operators write as YAML —
// but the file is parsed through encoding/json, so both forms must land on
// exactly the same table.
func TestParse_YAMLAndJSONAreEquivalent(t *testing.T) {
	fromYAML, err := Parse([]byte(yamlTable), noEnv())
	require.NoError(t, err)
	fromJSON, err := Parse([]byte(jsonTable), noEnv())
	require.NoError(t, err)

	assert.Equal(t, fromYAML.String(), fromJSON.String())
	assert.Equal(t, 2, fromYAML.Len())
}

func TestParse_ParsedShape(t *testing.T) {
	tbl, err := Parse([]byte(yamlTable), noEnv())
	require.NoError(t, err)

	// Backends are held in ascending name order — the merge order.
	bs := tbl.Backends()
	require.Len(t, bs, 2)
	assert.Equal(t, "netapp-a", bs[0].Name())
	assert.Equal(t, "zone-a", bs[1].Name())
	assert.Equal(t, []promql.Family{promql.FamilyHarvest}, bs[0].Families())
	assert.Equal(t, []string{"zone-a"}, bs[0].Zones())

	// Harvest is routed to its own installation; kube_* is not.
	assert.Equal(t, []string{"netapp-a"}, backendNamesOf(tbl.Select(promql.FamilyHarvest, []string{"zone-a"})))
	assert.Equal(t, []string{"zone-a"}, backendNamesOf(tbl.Select(promql.FamilyKSM, []string{"zone-a"})))
}

func backendNamesOf(bs []promql.Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name()
	}
	return out
}

// A misspelled field must be an error, not a silent no-op: `zone:` for
// `zones:` would otherwise turn a zone-scoped backend into a catch-all with
// no signal at all.
func TestParse_RejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
    zone: [zone-a]
`), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backends file")
}

func TestParse_RejectsEmptyAndUnparseable(t *testing.T) {
	_, err := Parse([]byte("backends: []"), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no backends")

	_, err = Parse([]byte("::: not yaml :::"), noEnv())
	require.Error(t, err)
}

func TestParse_RejectsUnknownFamily(t *testing.T) {
	_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [netapp]
`), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-a"`)
	assert.Contains(t, err.Error(), `unknown family "netapp"`)
}

// The structural rules live in promql.NewTable, so the file path and an
// embedder constructing a table directly are validated identically.
func TestParse_DelegatesStructuralValidation(t *testing.T) {
	_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
`), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `family "harvest" is served by no backend`)
}

// --- credentials ---------------------------------------------------------

const credTable = `
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
    usernameEnv: KSG_PROM_USERNAME_A
    passwordEnv: KSG_PROM_PASSWORD_A
`

func TestParse_PerBackendCredentials(t *testing.T) {
	tbl, err := Parse([]byte(credTable), env(map[string]string{
		"KSG_PROM_USERNAME_A": "ksg-a",
		"KSG_PROM_PASSWORD_A": "s3cret-a",
	}))
	require.NoError(t, err)
	u, p := tbl.Backends()[0].Credentials()
	assert.Equal(t, "ksg-a", u)
	assert.Equal(t, "s3cret-a", p)
}

// A backend naming no variables of its own inherits the global pair.
func TestParse_GlobalPairIsTheFallback(t *testing.T) {
	tbl, err := Parse([]byte(yamlTable), env(map[string]string{
		"KSG_PROM_USERNAME": "global",
		"KSG_PROM_PASSWORD": "global-pw",
	}))
	require.NoError(t, err)
	for _, b := range tbl.Backends() {
		u, p := b.Credentials()
		assert.Equal(t, "global", u)
		assert.Equal(t, "global-pw", p)
	}
}

// The per-backend pair wins over the global one for that backend only.
func TestParse_PerBackendOverridesGlobal(t *testing.T) {
	tbl, err := Parse([]byte(`
backends:
  - name: own
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
    usernameEnv: KSG_PROM_USERNAME_A
    passwordEnv: KSG_PROM_PASSWORD_A
  - name: inherits
    url: http://vm-netapp:8428
    families: [harvest]
`), env(map[string]string{
		"KSG_PROM_USERNAME":   "global",
		"KSG_PROM_PASSWORD":   "global-pw",
		"KSG_PROM_USERNAME_A": "own-user",
		"KSG_PROM_PASSWORD_A": "own-pw",
	}))
	require.NoError(t, err)

	byName := map[string][2]string{}
	for _, b := range tbl.Backends() {
		u, p := b.Credentials()
		byName[b.Name()] = [2]string{u, p}
	}
	assert.Equal(t, [2]string{"own-user", "own-pw"}, byName["own"])
	assert.Equal(t, [2]string{"global", "global-pw"}, byName["inherits"])
}

// A half-declared pair is always a mistake; guessing which half was meant
// would authenticate with an empty username or password.
func TestParse_RejectsHalfDeclaredPair(t *testing.T) {
	for _, body := range []string{
		"    usernameEnv: KSG_PROM_USERNAME_A\n",
		"    passwordEnv: KSG_PROM_PASSWORD_A\n",
	} {
		_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
`+body), env(map[string]string{
			"KSG_PROM_USERNAME_A": "u",
			"KSG_PROM_PASSWORD_A": "p",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `backend "zone-a"`)
		assert.Contains(t, err.Error(), "usernameEnv")
		assert.Contains(t, err.Error(), "passwordEnv")
		assert.NotContains(t, err.Error(), "s3cret")
	}
}

// A named-but-unset variable is a load failure, not a quiet fallback: a typo'd
// variable name would otherwise become 401s from one store, which — since a
// backend failure fails the whole query — points at the wrong thing.
func TestParse_RejectsUnsetNamedVariable(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"username unset": {"KSG_PROM_PASSWORD_A": "p"},
		"password unset": {"KSG_PROM_USERNAME_A": "u"},
		"username empty": {"KSG_PROM_USERNAME_A": "", "KSG_PROM_PASSWORD_A": "p"},
		"both unset":     {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(credTable), env(vars))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `backend "zone-a"`)
			assert.Contains(t, err.Error(), "unset or empty")
		})
	}
}

// The routing file is a ConfigMap; a credential value in it must be rejected
// with an error that says where credentials belong — and never echoes one.
func TestParse_RejectsLiteralCredentials(t *testing.T) {
	for _, field := range []string{"username: leaked-user", "password: leaked-pw"} {
		_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
    `+field+"\n"), noEnv())
		require.Error(t, err)
		assert.Contains(t, err.Error(), `backend "zone-a"`)
		assert.Contains(t, err.Error(), "usernameEnv")
		assert.NotContains(t, err.Error(), "leaked-user")
		assert.NotContains(t, err.Error(), "leaked-pw")
	}
}

// No error raised anywhere in the parse may echo a credential value.
func TestParse_ErrorsNeverEchoCredentials(t *testing.T) {
	const secret = "do-not-appear-in-any-error"
	_, err := Parse([]byte(`
backends:
  - name: zone-a
    url: not-a-url
    families: [ksm, kubelet, harvest, servicegraph, probe]
    usernameEnv: KSG_PROM_USERNAME_A
    passwordEnv: KSG_PROM_PASSWORD_A
`), env(map[string]string{
		"KSG_PROM_USERNAME_A": "u",
		"KSG_PROM_PASSWORD_A": secret,
	}))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

// --- file reading --------------------------------------------------------

func TestRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlTable), 0o600))

	tbl, err := Read(path, noEnv())
	require.NoError(t, err)
	assert.Equal(t, 2, tbl.Len())
}

func TestRead_MissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.yaml"), noEnv())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.yaml")
}

// --- documented example --------------------------------------------------

// The worked example in docs/upstream-backend-routing.md is tracked as
// testdata and parsed here, so a schema change that invalidates the
// documentation fails the build instead of leaving the docs quietly wrong.
func TestParse_DocumentedExampleParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "backends-example.yaml"))
	require.NoError(t, err)

	tbl, err := Parse(data, env(map[string]string{
		"KSG_PROM_USERNAME_ZONE_A": "ksg",
		"KSG_PROM_PASSWORD_ZONE_A": "s3cret",
	}))
	require.NoError(t, err)
	require.Equal(t, 3, tbl.Len())

	// The two claims the document makes about this example.
	assert.Equal(t, []string{"netapp"},
		backendNamesOf(tbl.Select(promql.FamilyHarvest, []string{"zone-a"})),
		"Harvest is served by its own installation, whatever the zone")
	assert.Equal(t, []string{"zone-a"},
		backendNamesOf(tbl.Select(promql.FamilyKSM, []string{"zone-a"})),
		"kube-state-metrics is zone-routed")
	assert.Equal(t, []string{"zone-a", "zone-b"},
		backendNamesOf(tbl.Select(promql.FamilyServiceGraph, []string{"zone-a"})),
		"the service-graph family is never narrowed by zone")
}
