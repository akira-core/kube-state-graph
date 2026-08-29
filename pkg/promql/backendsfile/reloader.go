package backendsfile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// ReloaderOptions configures a Reloader. Only Path is required.
//
// Logger and Metrics are OPTIONAL: nil means silent and unrecorded, so a module
// embedding the graph engine inherits neither kube-state-graph's log format nor
// its kube_state_graph_* series. That mirrors the no-op-tolerant build.Metrics /
// promql.Metrics the engine already takes (design D14).
type ReloaderOptions struct {
	// Path is the routing file to re-read.
	Path string
	// Lookup resolves the credential environment variables the file names.
	// Nil reads the process environment.
	Lookup LookupEnvFunc
	// Logger receives reload outcomes. Nil discards them.
	Logger *slog.Logger
	// Metrics counts reload attempts by result. Nil records nothing.
	Metrics promql.RouterMetrics
}

// Reloader re-reads the routing table on a ticker and swaps it into the live
// router when its content changed.
//
// Polling rather than fsnotify: a Kubernetes ConfigMap update replaces the
// ..data symlink rather than writing the file, so a watch has to be on the
// directory with a subtle event shape — and fsnotify would be a new direct
// dependency in a repository where that needs justification. One interval of
// latency is irrelevant for a topology change.
//
// It lives here rather than in cmd/ so an embedder arms the identical behaviour
// instead of re-deriving the digest short-circuit and the
// keep-the-previous-table rule (design D7 / D14).
type Reloader struct {
	router  *promql.Router
	path    string
	logger  *slog.Logger
	metrics promql.RouterMetrics
	lookup  LookupEnvFunc

	// lastDigest is the digest of the file content the live table was built
	// from, so an unchanged file costs one read and no re-parse.
	lastDigest [sha256.Size]byte
}

// NewReloader constructs a Reloader over the router serving the table that
// opts.Path was last parsed into.
//
// The digest is seeded from the file the caller's startup table was built from,
// so the first tick after boot does not re-parse an unchanged file.
func NewReloader(r *promql.Router, opts ReloaderOptions) *Reloader {
	rl := &Reloader{
		router:  r,
		path:    opts.Path,
		logger:  opts.Logger,
		metrics: opts.Metrics,
		lookup:  opts.Lookup,
	}
	if rl.logger == nil {
		rl.logger = discardLogger()
	}
	if rl.metrics == nil {
		rl.metrics = noopRouterMetrics{}
	}
	if data, err := os.ReadFile(opts.Path); err == nil { //nolint:gosec // operator-supplied config path
		rl.lastDigest = sha256.Sum256(data)
	}
	return rl
}

// Run re-reads the file every interval until ctx is cancelled.
func (rl *Reloader) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.Once()
		}
	}
}

// Once performs one reload tick.
//
// A file that fails to read, parse, or validate is rejected WHOLESALE: the
// previously live table keeps serving, the failure is logged and counted, and
// the next tick retries. Applying the valid subset of a broken file would
// silently route a family to fewer stores — a partial graph with no error.
//
// Run drives it from a bare goroutine with no recover above it, so a panic here
// would kill the whole process; recover, log, and let the next tick retry
// instead.
func (rl *Reloader) Once() {
	defer func() {
		if r := recover(); r != nil {
			rl.logger.Error("backends reload panicked",
				"path", rl.path,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			rl.metrics.IncBackendConfigReload(promql.ReloadResultError)
		}
	}()

	data, err := os.ReadFile(rl.path) //nolint:gosec // operator-supplied config path
	if err != nil {
		rl.logger.Error("backends reload failed", "path", rl.path, "err", err)
		rl.metrics.IncBackendConfigReload(promql.ReloadResultError)
		return
	}
	digest := sha256.Sum256(data)
	if digest == rl.lastDigest {
		rl.metrics.IncBackendConfigReload(promql.ReloadResultUnchanged)
		return
	}

	table, err := Parse(data, rl.lookup)
	if err != nil {
		// The digest is deliberately NOT advanced: the same broken content is
		// re-reported every interval until it is fixed, rather than failing
		// once and going quiet.
		rl.logger.Error("backends reload rejected; keeping the previous routing table",
			"path", rl.path, "err", err)
		rl.metrics.IncBackendConfigReload(promql.ReloadResultError)
		return
	}
	if err := rl.router.Swap(table); err != nil {
		rl.logger.Error("backends reload rejected; keeping the previous routing table",
			"path", rl.path, "err", err)
		rl.metrics.IncBackendConfigReload(promql.ReloadResultError)
		return
	}

	rl.lastDigest = digest
	rl.metrics.IncBackendConfigReload(promql.ReloadResultOK)
	rl.logger.Info("routing table reloaded", "path", rl.path, "backends", table.String())
}

// Start arms the reload loop when a path and a positive interval are both
// configured, and reports whether it did. A zero interval or an empty path
// leaves the startup table serving for the process lifetime.
func Start(ctx context.Context, r *promql.Router, opts ReloaderOptions, interval time.Duration) bool {
	if opts.Path == "" || interval <= 0 {
		return false
	}
	go NewReloader(r, opts).Run(ctx, interval)
	return true
}

// noopRouterMetrics is what a nil Metrics resolves to, so the reload path has
// no per-call nil check.
type noopRouterMetrics struct{}

func (noopRouterMetrics) SetBackends([]string)          {}
func (noopRouterMetrics) IncBackendQueryFailure(string) {}
func (noopRouterMetrics) IncBackendConfigReload(string) {}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
