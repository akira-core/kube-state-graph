package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noEnv(string) (string, bool) { return "", false }

const twoBackends = `
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-a]
  - name: netapp
    url: http://vm-netapp:8428
    families: [harvest]
`

const threeBackends = twoBackends + `
  - name: zone-b
    url: http://vm-b:8428
    families: [ksm, kubelet]
    zones: [zone-b]
`

func writeTable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backends.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// recordingMetrics captures the RouterMetrics upgrade so a reload test can
// assert on outcomes without scraping an exposition endpoint. It is
// mutex-guarded because the reload loop writes from its own goroutine while
// the test reads.
type recordingMetrics struct {
	mu       sync.Mutex
	backends []string
	reloads  []string
}

// The two required promql.Metrics methods, so one recorder can be handed to
// both the router and the reloader — exactly as main.go hands them the same
// *observability.Metrics.
func (r *recordingMetrics) ObserveQueryDuration(string, float64) {}
func (r *recordingMetrics) IncQueryFailure(string)               {}

func (r *recordingMetrics) SetBackends(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends = names
}

func (r *recordingMetrics) IncBackendQueryFailure(string) {}

func (r *recordingMetrics) IncBackendConfigReload(x string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads = append(r.reloads, x)
}

func (r *recordingMetrics) count(result string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.reloads {
		if x == result {
			n++
		}
	}
	return n
}

func (r *recordingMetrics) reloadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reloads)
}

func (r *recordingMetrics) backendNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.backends...)
}

// --- initial table -------------------------------------------------------

// With no routing file the implicit single backend serves every family, so the
// deployment behaves exactly as it did before backend routing existed.
func TestInitialTable_ImplicitSingleBackend(t *testing.T) {
	cfg := config.Defaults()
	cfg.PromURL = "http://vm.example:8428"

	tbl, err := initialTable(cfg, quietLogger(), noEnv)
	require.NoError(t, err)
	require.Equal(t, 1, tbl.Len())

	b := tbl.Backends()[0]
	assert.Equal(t, config.DefaultBackendName, b.Name())
	assert.Equal(t, "http://vm.example:8428", b.URL())
	assert.Len(t, b.Families(), 5)
	assert.Empty(t, b.Zones())
}

func TestInitialTable_ImplicitBackendCarriesGlobalCredentials(t *testing.T) {
	cfg := config.Defaults()
	cfg.PromUsername = "u"
	cfg.PromPassword = "p"

	tbl, err := initialTable(cfg, quietLogger(), noEnv)
	require.NoError(t, err)
	u, p := tbl.Backends()[0].Credentials()
	assert.Equal(t, "u", u)
	assert.Equal(t, "p", p)
}

func TestInitialTable_FromFile(t *testing.T) {
	cfg := config.Defaults()
	cfg.BackendsFile = writeTable(t, twoBackends)

	tbl, err := initialTable(cfg, quietLogger(), noEnv)
	require.NoError(t, err)
	assert.Equal(t, 2, tbl.Len())
}

// An invalid file at BOOT is fatal, unlike the reload path: there is no
// previously-good table to fall back to.
func TestInitialTable_InvalidFileIsFatal(t *testing.T) {
	cfg := config.Defaults()
	cfg.BackendsFile = writeTable(t, "backends:\n  - name: broken\n    url: not-a-url\n    families: [ksm]\n")

	_, err := initialTable(cfg, quietLogger(), noEnv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "broken"`)
}

func TestInitialTable_MissingFileIsFatal(t *testing.T) {
	cfg := config.Defaults()
	cfg.BackendsFile = filepath.Join(t.TempDir(), "absent.yaml")

	_, err := initialTable(cfg, quietLogger(), noEnv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.yaml")
}

func TestBuildRouter_ImplicitBackendRoutesEveryFamily(t *testing.T) {
	cfg := config.Defaults()
	r, err := buildRouter(cfg, nil, quietLogger(), noEnv)
	require.NoError(t, err)
	require.Equal(t, 1, r.Table().Len())

	for _, f := range promql.Families {
		sel := r.Table().Select(f, []string{"zone-a"})
		require.Len(t, sel, 1, "family %q must have exactly one destination", f)
		assert.Equal(t, config.DefaultBackendName, sel[0].Name())
	}
}

// --- reload --------------------------------------------------------------

func newTestReloader(t *testing.T, path string, m *recordingMetrics) (*promql.Router, *backendReloader) {
	t.Helper()
	tbl, err := config.ReadBackendsFile(path, noEnv)
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, m, func(promql.Backend) (promql.Querier, error) {
		return promqlNoopQuerier{}, nil
	})
	require.NoError(t, err)
	return r, newBackendReloader(r, path, quietLogger(), m, noEnv)
}

// promqlNoopQuerier stands in for an upstream client: the reload tests care
// about which table is live, never about query results.
type promqlNoopQuerier struct{}

func (promqlNoopQuerier) Instant(context.Context, string, string, time.Time) (model.Vector, error) {
	return model.Vector{}, nil
}

func TestReloader_SwapsWhenTheFileChanges(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m)
	require.Equal(t, 2, r.Table().Len())

	// An unchanged file costs one read and no swap.
	rl.once()
	assert.Equal(t, 2, r.Table().Len())
	assert.Equal(t, 1, m.count(promql.ReloadResultUnchanged))
	assert.Zero(t, m.count(promql.ReloadResultOK))

	require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
	rl.once()
	assert.Equal(t, 3, r.Table().Len(), "the new backend serves without a restart")
	assert.Equal(t, 1, m.count(promql.ReloadResultOK))
	assert.Equal(t, []string{"netapp", "zone-a", "zone-b"}, m.backendNames())
}

// A file that fails to parse or validate is rejected WHOLESALE: the previously
// live table keeps serving and the failure is counted.
func TestReloader_InvalidFileKeepsTheLiveTable(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m)

	for _, bad := range []string{
		"::: not yaml :::",
		"backends:\n  - name: broken\n    url: not-a-url\n    families: [ksm]\n",
		"backends: []",
	} {
		require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))
		rl.once()
		assert.Equal(t, 2, r.Table().Len(), "the previous table still serves after %q", bad)
	}
	assert.Equal(t, 3, m.count(promql.ReloadResultError))
	assert.Zero(t, m.count(promql.ReloadResultOK))
}

// The digest is not advanced on a rejected file, so the same broken content is
// re-reported every interval rather than failing once and going quiet.
func TestReloader_BrokenFileIsReReportedEveryTick(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	_, rl := newTestReloader(t, path, m)

	require.NoError(t, os.WriteFile(path, []byte("backends: []"), 0o600))
	rl.once()
	rl.once()
	rl.once()
	assert.Equal(t, 3, m.count(promql.ReloadResultError))
	assert.Zero(t, m.count(promql.ReloadResultUnchanged), "a rejected file never counts as unchanged")
}

func TestReloader_UnreadableFileKeepsTheLiveTable(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m)

	require.NoError(t, os.Remove(path))
	rl.once()
	assert.Equal(t, 2, r.Table().Len())
	assert.Equal(t, 1, m.count(promql.ReloadResultError))
}

// A zero interval starts no goroutine: the table read at startup serves for the
// process lifetime.
func TestStartBackendReload_DisabledByZeroInterval(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, _ := newTestReloader(t, path, m)

	cfg := config.Defaults()
	cfg.BackendsFile = path
	cfg.BackendsReloadInterval = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startBackendReload(ctx, r, cfg, quietLogger(), m, noEnv)

	require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
	// Give any (incorrectly) started goroutine ample opportunity to fire.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 2, r.Table().Len(), "no reload goroutine may run at a zero interval")
	assert.Zero(t, m.reloadCount())
}

// With no routing file there is nothing to reload, whatever the interval.
func TestStartBackendReload_DisabledWithoutAFile(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, _ := newTestReloader(t, path, m)

	cfg := config.Defaults()
	cfg.BackendsFile = ""
	cfg.BackendsReloadInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startBackendReload(ctx, r, cfg, quietLogger(), m, noEnv)

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, m.reloadCount())
}

// The loop stops with its context so shutdown stays deterministic.
func TestStartBackendReload_StopsWithContext(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, _ := newTestReloader(t, path, m)

	cfg := config.Defaults()
	cfg.BackendsFile = path
	cfg.BackendsReloadInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	startBackendReload(ctx, r, cfg, quietLogger(), m, noEnv)
	require.Eventually(t, func() bool { return m.reloadCount() > 0 }, time.Second, 5*time.Millisecond)
	cancel()

	time.Sleep(50 * time.Millisecond)
	settled := m.reloadCount()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, settled, m.reloadCount(), "the loop stops with its context")
}

// capturingLogger returns a logger writing JSON into buf, so a test can assert
// on what startup told the operator.
func capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// When both are configured the file wins, and the operator is told — silently
// ignoring an explicitly-set --prom-url is exactly the kind of quiet override
// that costs an afternoon.
func TestInitialTable_FileOverridesPromURLAndSaysSo(t *testing.T) {
	cfg := config.Defaults()
	cfg.PromURL = "http://explicitly-set.example:8428"
	cfg.BackendsFile = writeTable(t, twoBackends)

	var buf bytes.Buffer
	tbl, err := initialTable(cfg, capturingLogger(&buf), noEnv)
	require.NoError(t, err)
	assert.Equal(t, 2, tbl.Len(), "the file's backends serve, not --prom-url")

	log := buf.String()
	assert.Contains(t, log, "--prom-url is ignored because --backends-file is set")
	assert.Contains(t, log, "http://explicitly-set.example:8428")

	// The file's own backends never include one addressed at --prom-url.
	for _, b := range tbl.Backends() {
		assert.NotEqual(t, cfg.PromURL, b.URL())
	}
}

// A --prom-url left at its default is not an operator choice, so it must not
// produce a warning nobody can act on.
func TestInitialTable_DefaultPromURLDoesNotWarn(t *testing.T) {
	cfg := config.Defaults()
	cfg.BackendsFile = writeTable(t, twoBackends)

	var buf bytes.Buffer
	_, err := initialTable(cfg, capturingLogger(&buf), noEnv)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "--prom-url is ignored")
}

// The startup log names the backends but never a credential value.
func TestInitialTable_StartupLogNeverCarriesCredentials(t *testing.T) {
	const secret = "s3cret-must-not-be-logged"
	cfg := config.Defaults()
	cfg.BackendsFile = writeTable(t, `
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, harvest, servicegraph, probe]
    usernameEnv: KSG_PROM_USERNAME_A
    passwordEnv: KSG_PROM_PASSWORD_A
`)

	var buf bytes.Buffer
	_, err := initialTable(cfg, capturingLogger(&buf), func(k string) (string, bool) {
		switch k {
		case "KSG_PROM_USERNAME_A":
			return "ksg", true
		case "KSG_PROM_PASSWORD_A":
			return secret, true
		}
		return "", false
	})
	require.NoError(t, err)

	log := buf.String()
	assert.Contains(t, log, "zone-a", "the backend is named")
	assert.Contains(t, log, "auth=true", "and its auth state reported")
	assert.NotContains(t, log, secret)
	assert.NotContains(t, log, "ksg-upstream", "no credential value reaches the log")
}
