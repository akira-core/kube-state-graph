package backendsfile

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// recordingMetrics captures the RouterMetrics upgrade so a reload test can
// assert on outcomes without scraping an exposition endpoint. It is
// mutex-guarded because the reload loop writes from its own goroutine while
// the test reads.
type recordingMetrics struct {
	mu       sync.Mutex
	backends []string
	reloads  []string
}

func (r *recordingMetrics) ObserveQueryDuration(string, float64) {}
func (r *recordingMetrics) IncQueryFailure(string)               {}
func (r *recordingMetrics) IncBackendQueryFailure(string)        {}

func (r *recordingMetrics) SetBackends(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends = names
}

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

// noopQuerier stands in for an upstream client: the reload tests care about
// which table is live, never about query results.
type noopQuerier struct{}

func (noopQuerier) Instant(context.Context, string, string, time.Time) (model.Vector, error) {
	return model.Vector{}, nil
}

func newTestReloader(t *testing.T, path string, m *recordingMetrics, logger *slog.Logger) (*promql.Router, *Reloader) {
	t.Helper()
	tbl, err := Read(path, noEnv())
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, m, func(promql.Backend) (promql.Querier, error) {
		return noopQuerier{}, nil
	})
	require.NoError(t, err)
	return r, NewReloader(r, ReloaderOptions{Path: path, Lookup: noEnv(), Logger: logger, Metrics: m})
}

func TestReloader_SwapsWhenTheFileChanges(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m, quietLogger())
	require.Equal(t, 2, r.Table().Len())

	// An unchanged file costs one read and no swap.
	rl.Once()
	assert.Equal(t, 2, r.Table().Len())
	assert.Equal(t, 1, m.count(promql.ReloadResultUnchanged))
	assert.Zero(t, m.count(promql.ReloadResultOK))

	require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
	rl.Once()
	assert.Equal(t, 3, r.Table().Len(), "the new backend serves without a restart")
	assert.Equal(t, 1, m.count(promql.ReloadResultOK))
	assert.Equal(t, []string{"netapp", "zone-a", "zone-b"}, m.backendNames())
}

// A file that fails to parse or validate is rejected WHOLESALE: the previously
// live table keeps serving and the failure is counted.
func TestReloader_InvalidFileKeepsTheLiveTable(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m, quietLogger())

	for _, bad := range []string{
		"::: not yaml :::",
		"backends:\n  - name: broken\n    url: not-a-url\n    families: [ksm]\n",
		"backends: []",
	} {
		require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))
		rl.Once()
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
	_, rl := newTestReloader(t, path, m, quietLogger())

	require.NoError(t, os.WriteFile(path, []byte("backends: []"), 0o600))
	rl.Once()
	rl.Once()
	rl.Once()
	assert.Equal(t, 3, m.count(promql.ReloadResultError))
	assert.Zero(t, m.count(promql.ReloadResultUnchanged), "a rejected file never counts as unchanged")
}

func TestReloader_UnreadableFileKeepsTheLiveTable(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, rl := newTestReloader(t, path, m, quietLogger())

	require.NoError(t, os.Remove(path))
	rl.Once()
	assert.Equal(t, 2, r.Table().Len())
	assert.Equal(t, 1, m.count(promql.ReloadResultError))
}

// An embedder supplying neither a logger nor a recorder inherits neither
// kube-state-graph's log format nor its self-metrics — and the loop still
// reloads (design D14).
func TestReloader_NilLoggerAndMetricsAreSilentButStillReload(t *testing.T) {
	path := writeTable(t, twoBackends)
	tbl, err := Read(path, noEnv())
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(promql.Backend) (promql.Querier, error) {
		return noopQuerier{}, nil
	})
	require.NoError(t, err)

	rl := NewReloader(r, ReloaderOptions{Path: path, Lookup: noEnv()})

	require.NotPanics(t, func() {
		rl.Once() // unchanged
		require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
		rl.Once() // accepted
		require.NoError(t, os.WriteFile(path, []byte("backends: []"), 0o600))
		rl.Once() // rejected
		require.NoError(t, os.Remove(path))
		rl.Once() // unreadable
	})
	assert.Equal(t, 3, r.Table().Len(), "the accepted table serves; the rejected one never did")
}

// The default logger writes nowhere, so a nil-logger reload emits no output on
// the caller's own handler either.
func TestReloader_NilLoggerWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	path := writeTable(t, twoBackends)
	tbl, err := Read(path, noEnv())
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(promql.Backend) (promql.Querier, error) {
		return noopQuerier{}, nil
	})
	require.NoError(t, err)

	rl := NewReloader(r, ReloaderOptions{Path: path, Lookup: noEnv()})
	require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
	rl.Once()

	assert.Empty(t, buf.String(), "a nil logger must not fall back to the default one")
}

// --- Start ---------------------------------------------------------------

func TestStart_RunsUntilTheContextIsCancelled(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, _ := newTestReloader(t, path, m, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	armed := Start(ctx, r, ReloaderOptions{Path: path, Lookup: noEnv(), Logger: quietLogger(), Metrics: m}, 5*time.Millisecond)
	require.True(t, armed)

	require.Eventually(t, func() bool { return m.reloadCount() > 0 }, time.Second, 5*time.Millisecond)
	cancel()

	require.Eventually(t, func() bool {
		settled := m.reloadCount()
		time.Sleep(30 * time.Millisecond)
		return settled == m.reloadCount()
	}, time.Second, 30*time.Millisecond, "the loop stops with its context")
}

func TestStart_NotArmedWithoutAPathOrInterval(t *testing.T) {
	path := writeTable(t, twoBackends)
	m := &recordingMetrics{}
	r, _ := newTestReloader(t, path, m, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	assert.False(t, Start(ctx, r, ReloaderOptions{Path: path, Metrics: m}, 0))
	assert.False(t, Start(ctx, r, ReloaderOptions{Path: "", Metrics: m}, time.Millisecond))

	require.NoError(t, os.WriteFile(path, []byte(threeBackends), 0o600))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 2, r.Table().Len())
	assert.Zero(t, m.reloadCount())
}
