package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const urlSecret = "s3cret-in-userinfo"

// A routing-file url carrying userinfo is rendered with its password
// redacted in the table's log form, while the dial target stays raw.
func TestBackend_StringRedactsURLUserinfo(t *testing.T) {
	raw := "http://ksg:" + urlSecret + "@vm-a:8428/select/0/prometheus"
	tbl, err := NewTable([]Backend{be("zone-a", raw, allFamilies())})
	require.NoError(t, err)

	for _, rendered := range []string{tbl.String(), tbl.Backends()[0].String()} {
		assert.NotContains(t, rendered, urlSecret)
		assert.Contains(t, rendered, "url=http://ksg:xxxxx@vm-a:8428/select/0/prometheus")
	}
	assert.Equal(t, raw, tbl.Backends()[0].URL(), "URL() is the dial target and stays raw")
}

// A credential-free URL renders exactly as before.
func TestBackend_StringKeepsPlainURL(t *testing.T) {
	b := be("zone-a", "http://vm-a:8428", allFamilies())
	assert.Contains(t, b.String(), "url=http://vm-a:8428 ")
}

// Every NewTable rejection of a bad URL names the backend and never the
// password — a rejected routing file is re-reported on every reload tick.
func TestNewTable_InvalidURLErrorsNeverEchoUserinfo(t *testing.T) {
	for _, raw := range []string{
		"http://ksg:" + urlSecret + "@vm-a:84x28",  // unparseable port
		"http://ksg:" + urlSecret + "%zz@vm-a",     // bad escape
		"http://ksg:" + urlSecret + "@",            // no host
		"http:/ksg:" + urlSecret + "@vm-a:8428",    // one slash: credentials in the path
		"ftp://ksg:" + urlSecret + "@vm-a:8428",    // wrong scheme
		"ksg:" + urlSecret + "@vm-a:8428",          // opaque
		"//ksg:" + urlSecret + "@vm-a:8428/select", // no scheme
	} {
		_, err := NewTable([]Backend{be("zone-a", raw, allFamilies())})
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), `backend "zone-a"`, raw)
		assert.NotContains(t, err.Error(), urlSecret, raw)
	}
}

func TestNew_ParseErrorNeverEchoesUserinfo(t *testing.T) {
	_, err := New("http://ksg:"+urlSecret+"@vm-a:84x28", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), urlSecret)
}
