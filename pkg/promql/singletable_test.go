package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func backendNamesOf(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name()
	}
	return out
}

// --- implicit single-backend table ---------------------------------------

// The compatibility mode is a TABLE, not a separate unrouted code path, so
// every existing test exercises the router in its degenerate configuration.
func TestSingleBackendTable(t *testing.T) {
	tbl, err := SingleBackendTable("http://vm.example:8428", "", "")
	require.NoError(t, err)
	require.Equal(t, 1, tbl.Len())

	b := tbl.Backends()[0]
	assert.Equal(t, DefaultBackendName, b.Name())
	assert.Equal(t, "http://vm.example:8428", b.URL())
	assert.Len(t, b.Families(), 5, "the implicit backend serves every family")
	assert.Empty(t, b.Zones(), "the implicit backend is a catch-all")

	// Every family resolves to exactly one destination, zone-scoped or not.
	for _, f := range Families {
		assert.Equal(t, []string{DefaultBackendName}, backendNamesOf(tbl.Select(f, nil)))
		assert.Equal(t, []string{DefaultBackendName}, backendNamesOf(tbl.Select(f, []string{"zone-a"})))
	}
}

func TestSingleBackendTable_CarriesGlobalCredentials(t *testing.T) {
	tbl, err := SingleBackendTable("http://vm.example:8428", "u", "p")
	require.NoError(t, err)
	u, p := tbl.Backends()[0].Credentials()
	assert.Equal(t, "u", u)
	assert.Equal(t, "p", p)
}

func TestSingleBackendTable_RejectsBadURL(t *testing.T) {
	_, err := SingleBackendTable("not-a-url", "", "")
	require.Error(t, err)
}
