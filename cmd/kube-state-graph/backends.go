package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// buildRouter constructs the upstream router from the configured routing table,
// or from the implicit single-backend table when none is configured.
//
// The implicit table is not a separate unrouted code path: a deployment with
// only --prom-url routes through exactly the same dispatch, with one candidate
// per query, so every rendered query string and response body is byte-identical
// to the same deployment before backend routing existed.
func buildRouter(cfg config.Config, m promql.Metrics, logger *slog.Logger, lookup config.LookupEnvFunc) (*promql.Router, error) {
	table, err := initialTable(cfg, logger, lookup)
	if err != nil {
		return nil, err
	}
	r, err := promql.NewRouter(table, m, promql.DefaultClientFactory(m))
	if err != nil {
		return nil, err
	}
	return r, nil
}

func initialTable(cfg config.Config, logger *slog.Logger, lookup config.LookupEnvFunc) (*promql.Table, error) {
	if cfg.BackendsFile == "" {
		table, err := config.SingleBackendTable(cfg.PromURL, cfg.PromUsername, cfg.PromPassword)
		if err != nil {
			return nil, err
		}
		logger.Info("upstream routing disabled — serving from a single implicit backend",
			"backend", config.DefaultBackendName,
			"prom_url", cfg.PromURL,
		)
		return table, nil
	}

	// An invalid file at BOOT is fatal, unlike the reload path: there is no
	// previously-good table to fall back to, and starting on a guess would
	// route queries somewhere the operator did not ask for.
	table, err := config.ReadBackendsFile(cfg.BackendsFile, lookup)
	if err != nil {
		return nil, err
	}
	if cfg.PromURL != "" && cfg.PromURL != config.Defaults().PromURL {
		logger.Warn("--prom-url is ignored because --backends-file is set",
			"prom_url", cfg.PromURL,
			"backends_file", cfg.BackendsFile,
		)
	}
	// The table's rendered form names backends, endpoints, families and zones,
	// and reports only WHETHER credentials are configured — never a value.
	logger.Info("upstream routing enabled",
		"backends_file", cfg.BackendsFile,
		"reload_interval", cfg.BackendsReloadInterval,
		"backends", table.String(),
	)
	return table, nil
}

// backendReloader re-reads the routing table on a ticker and swaps it into the
// live router when its content changed.
//
// Polling rather than fsnotify: a Kubernetes ConfigMap update replaces the
// ..data symlink rather than writing the file, so a watch has to be on the
// directory with a subtle event shape — and fsnotify would be a new direct
// dependency in a repository where that needs justification. One interval of
// latency is irrelevant for a topology change.
type backendReloader struct {
	router  *promql.Router
	path    string
	logger  *slog.Logger
	metrics promql.RouterMetrics
	lookup  config.LookupEnvFunc

	// lastDigest is the digest of the file content the live table was built
	// from, so an unchanged file costs one read and no re-parse.
	lastDigest [sha256.Size]byte
}

func newBackendReloader(r *promql.Router, path string, logger *slog.Logger, m promql.RouterMetrics, lookup config.LookupEnvFunc) *backendReloader {
	rl := &backendReloader{router: r, path: path, logger: logger, metrics: m, lookup: lookup}
	// Seed the digest from the file the startup table was built from, so the
	// first tick after boot does not re-parse an unchanged file.
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // operator-supplied config path
		rl.lastDigest = sha256.Sum256(data)
	}
	return rl
}

func (rl *backendReloader) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.once()
		}
	}
}

// once performs one reload tick.
//
// A file that fails to read, parse, or validate is rejected WHOLESALE: the
// previously live table keeps serving, the failure is logged and counted, and
// the next tick retries. Applying the valid subset of a broken file would
// silently route a family to fewer stores — a partial graph with no error.
//
// It runs on a bare goroutine with no recover above it, so a panic here would
// kill the whole process; recover, log, and let the next tick retry instead.
func (rl *backendReloader) once() {
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

	table, err := config.ParseBackendsFile(data, rl.lookup)
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

// startBackendReload arms the reload loop when a routing file and a positive
// interval are both configured. A zero interval leaves the startup table
// serving for the process lifetime.
func startBackendReload(ctx context.Context, r *promql.Router, cfg config.Config, logger *slog.Logger, m promql.RouterMetrics, lookup config.LookupEnvFunc) {
	if cfg.BackendsFile == "" || cfg.BackendsReloadInterval <= 0 {
		return
	}
	go newBackendReloader(r, cfg.BackendsFile, logger, m, lookup).run(ctx, cfg.BackendsReloadInterval)
}
