package auth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeySet_EmptyByDefault(t *testing.T) {
	ks := NewKeySet()
	assert.True(t, ks.Empty())
	assert.False(t, ks.Validate("anything"))
	assert.Equal(t, 0, ks.Snapshot())
}

func TestKeySet_LoadCSV(t *testing.T) {
	ks := NewKeySet()
	ks.LoadCSV("alpha, beta ,gamma,,alpha")
	assert.False(t, ks.Empty())
	assert.Equal(t, 3, ks.Snapshot(), "duplicates and blanks dropped")
	assert.True(t, ks.Validate("alpha"))
	assert.True(t, ks.Validate("beta"))
	assert.True(t, ks.Validate("gamma"))
	assert.False(t, ks.Validate("delta"))
	assert.False(t, ks.Validate(""))
}

func TestKeySet_LoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte(`
# comment
alpha
beta

# blank-line tolerant
gamma
alpha
`), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))
	assert.Equal(t, 3, ks.Snapshot())
	assert.True(t, ks.Validate("alpha"))
	assert.True(t, ks.Validate("beta"))
	assert.True(t, ks.Validate("gamma"))
	assert.False(t, ks.Validate("# comment"))
}

func TestKeySet_Reload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))
	assert.True(t, ks.Validate("k1"))
	assert.False(t, ks.Validate("k2"))

	require.NoError(t, os.WriteFile(path, []byte("k2\n"), 0o600))
	require.NoError(t, ks.LoadFile(path))
	assert.False(t, ks.Validate("k1"), "old key revoked after reload")
	assert.True(t, ks.Validate("k2"))
}

func TestKeySet_LoadFile_Missing(t *testing.T) {
	ks := NewKeySet()
	assert.Error(t, ks.LoadFile("/nonexistent/path/keys"))
}

func TestKeySet_RejectsEmptyPresented(t *testing.T) {
	ks := NewKeySet()
	ks.LoadCSV("k1")
	assert.False(t, ks.Validate(""))
}

func TestKeySet_ReloadFile_RefusesEmptyOverActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\nk2\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))
	require.Equal(t, 2, ks.Snapshot())

	// Simulate a truncated / comment-only file mid-rotation.
	require.NoError(t, os.WriteFile(path, []byte("# rotated, keys not yet written\n"), 0o600))

	changed, err := ks.ReloadFile(path)
	require.Error(t, err, "reload must refuse to fail open")
	assert.False(t, changed)
	assert.Contains(t, err.Error(), "refusing to replace")

	// Old keys MUST still be active — auth was not silently disabled.
	assert.Equal(t, 2, ks.Snapshot())
	assert.True(t, ks.Validate("k1"))
	assert.True(t, ks.Validate("k2"))
	assert.False(t, ks.Empty())
}

func TestKeySet_ReloadFile_EmptyOverEmptyIsFine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("# no keys\n"), 0o600))

	ks := NewKeySet()
	changed, err := ks.ReloadFile(path)
	require.NoError(t, err, "empty over empty must not error")
	assert.False(t, changed, "empty over empty is not a change")
	assert.True(t, ks.Empty())
	assert.Equal(t, 0, ks.Snapshot())
}

func TestKeySet_ReloadFile_SwapsNonEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))
	assert.True(t, ks.Validate("k1"))

	require.NoError(t, os.WriteFile(path, []byte("k2\nk3\n"), 0o600))
	changed, err := ks.ReloadFile(path)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.False(t, ks.Validate("k1"), "old key revoked after reload")
	assert.True(t, ks.Validate("k2"))
	assert.True(t, ks.Validate("k3"))
	assert.Equal(t, 2, ks.Snapshot())
}

// The common rotation shape replaces N keys with N different ones — the count
// stays constant, so the changed flag must come from set comparison, not
// Snapshot() counts.
func TestKeySet_ReloadFile_SameCountRotationIsChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\nk2\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))

	require.NoError(t, os.WriteFile(path, []byte("k3\nk4\n"), 0o600))
	changed, err := ks.ReloadFile(path)
	require.NoError(t, err)
	assert.True(t, changed, "N→N rotation must report changed")
	assert.False(t, ks.Validate("k1"))
	assert.True(t, ks.Validate("k3"))
	assert.True(t, ks.Validate("k4"))
}

func TestKeySet_ReloadFile_IdenticalContentNotChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\nk2\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))

	changed, err := ks.ReloadFile(path)
	require.NoError(t, err)
	assert.False(t, changed, "identical content must not report changed")
}

func TestKeySet_ReloadFile_ReorderOnlyNotChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\nk2\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))

	require.NoError(t, os.WriteFile(path, []byte("k2\nk1\n"), 0o600))
	changed, err := ks.ReloadFile(path)
	require.NoError(t, err)
	assert.False(t, changed, "set semantics: reorder-only rewrite is not a change")
	assert.True(t, ks.Validate("k1"))
	assert.True(t, ks.Validate("k2"))
}

func TestKeySet_ReloadFile_ReadErrorKeepsActiveKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	require.NoError(t, os.WriteFile(path, []byte("k1\n"), 0o600))

	ks := NewKeySet()
	require.NoError(t, ks.LoadFile(path))

	changed, err := ks.ReloadFile(filepath.Join(dir, "missing"))
	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, ks.Validate("k1"), "active keys retained on read failure")
}
