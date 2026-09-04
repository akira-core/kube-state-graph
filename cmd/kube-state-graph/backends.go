package main

import (
	"context"
	"log/slog"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
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
	logUnservedFamilies(table, logger)
	r, err := promql.NewRouter(table, m, promql.DefaultClientFactory(m))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// logUnservedFamilies reports each OPTIONAL family the live table leaves
// served by no backend. A required family can never appear here — NewTable
// rejects such a table — so this is purely the operator's confirmation that a
// feature they did not configure is off, rather than silently broken. It is
// Info, not Warn: an unserved optional family is a choice, not a defect.
func logUnservedFamilies(table *promql.Table, logger *slog.Logger) {
	for _, f := range table.Unserved() {
		switch f {
		case promql.FamilyAlerts:
			logger.Info("alert overlay disabled — no backend serves the alerts family",
				"family", string(f),
			)
		default:
			logger.Info("optional upstream family is served by no backend",
				"family", string(f),
			)
		}
	}
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

// startBackendReload arms the reload loop when a routing file and a positive
// interval are both configured. A zero interval leaves the startup table
// serving for the process lifetime.
//
// The loop itself lives in pkg/promql/backendsfile, so an embedding module arms
// the identical behaviour instead of re-deriving the digest short-circuit and
// the keep-the-previous-table rule (design D14).
func startBackendReload(ctx context.Context, r *promql.Router, cfg config.Config, logger *slog.Logger, m promql.RouterMetrics, lookup config.LookupEnvFunc) {
	backendsfile.Start(ctx, r, backendsfile.ReloaderOptions{
		Path:    cfg.BackendsFile,
		Lookup:  backendsfile.LookupEnvFunc(lookup),
		Logger:  logger,
		Metrics: m,
	}, cfg.BackendsReloadInterval)
}
