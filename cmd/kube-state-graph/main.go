// Command kube-state-graph runs the multi-cluster pod / node graph API server.
//
//	@title			kube-state-graph API
//	@version		v1
//	@description	Multi-cluster pod / node / PVC graph API. Reads kube-state-metrics and pod-UID-resolved service-graph metrics from a centralised VictoriaMetrics and returns the joined cross-cluster graph as Cytoscape.js JSON.
//	@description
//	@description	**Authentication.** When the server is started with API keys configured (`--api-keys-file` or `--api-keys`), every request to `/v1/*` MUST carry an `X-API-Key: <key>` header. Missing or invalid keys yield `401 Unauthorized`. Health probes (`/livez`, `/readyz`), the metrics endpoint (`/metrics`), and the OpenAPI / Scalar UI routes (`/openapi.*`, `/docs`) are exempt and require no key.
//	@license.name	Apache 2.0
//	@license.url	https://www.apache.org/licenses/LICENSE-2.0.html
//	@BasePath		/
//	@host			localhost:8080
//
//	@securityDefinitions.apikey	ApiKeyAuth
//	@in							header
//	@name						X-API-Key
//	@description				API key presented in the `X-API-Key` header. Required on `/v1/*` when the server is started with keys configured. Health, metrics, and docs routes are exempt.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/marz32one/kube-state-graph/internal/api"
	"github.com/marz32one/kube-state-graph/internal/auth"
	"github.com/marz32one/kube-state-graph/internal/config"
	"github.com/marz32one/kube-state-graph/internal/observability"
	"github.com/marz32one/kube-state-graph/internal/telemetry"
	"github.com/marz32one/kube-state-graph/pkg/build"
	"github.com/marz32one/kube-state-graph/pkg/promql"
)

// version is the build-time service version. Override with
// `go build -ldflags "-X main.version=<v>"` at release time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.LookupEnv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// appCtx bounds background goroutines (e.g. the API-key reload loop) to the
	// process lifecycle; defer-cancel stops them on any return path so graceful
	// shutdown is deterministic rather than relying on process exit.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	telemetryProviders, telErr := telemetry.Init(bootCtx, version)
	bootCancel()
	if telErr != nil {
		// Telemetry init failure is non-fatal: fall back to local-only logs so
		// the binary still serves traffic when the OTel collector is missing.
		fmt.Fprintf(os.Stderr, "telemetry init failed (continuing without OTLP exports): %v\n", telErr)
	}

	localHandler := observability.NewLogHandler(cfg.LogLevel)
	logger := slog.New(telemetry.NewSlogHandler(localHandler))
	slog.SetDefault(logger)
	logger.Info("starting kube-state-graph",
		"prom_url", cfg.PromURL,
		"listen_addr", cfg.ListenAddr,
		"build_timeout", cfg.BuildTimeout,
		"api_timeout", cfg.APITimeout,
		"metric_prefix", cfg.MetricPrefix,
		"otlp_enabled", telemetryProviders.Enabled,
		// Boolean only — the credential values themselves are never logged.
		"prom_basic_auth", cfg.PromUsername != "",
	)

	metrics := observability.NewMetrics()
	// Upstream basic auth is env-only (KSG_PROM_USERNAME / KSG_PROM_PASSWORD);
	// config.Validate guarantees the pair is set together or not at all. The
	// startup log above must never carry the credential values.
	var promOpts []promql.Option
	if cfg.PromUsername != "" {
		promOpts = append(promOpts, promql.WithBasicAuth(cfg.PromUsername, cfg.PromPassword))
	}
	promClient, err := promql.New(cfg.PromURL, metrics, promOpts...)
	if err != nil {
		return fmt.Errorf("promql client: %w", err)
	}

	keys, err := loadAPIKeys(appCtx, cfg, logger)
	if err != nil {
		return fmt.Errorf("api keys: %w", err)
	}

	builder := build.New(promClient, build.Options{MetricPrefix: cfg.MetricPrefix, APITimeout: cfg.APITimeout}, metrics, nil)
	server := api.New(cfg, builder, promClient, metrics, logger, keys, nil)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.BuildTimeout + 5*time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		logger.Error("http server failed", "err", err)
		return err
	}

	// The drain window must cover the slowest legitimate in-flight request, so
	// derive it from the server's own WriteTimeout (with a 10s floor for very
	// short build timeouts) instead of re-deriving the formula.
	shutdownTimeout := max(httpSrv.WriteTimeout, 10*time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Telemetry shutdown MUST run even when the HTTP drain fails — otherwise
	// buffered OTLP spans/logs are dropped. Collect both errors.
	var errs []error
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown failed", "err", err)
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	// The telemetry flush gets its OWN budget: a timed-out drain has exhausted
	// shutdownCtx, and the OTel SDK Shutdowns bail immediately on an expired
	// context — exporting nothing in exactly the abnormal-shutdown case whose
	// spans/logs matter most (including the drain-failure record just emitted).
	telCtx, telCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer telCancel()
	if err := telemetryProviders.Shutdown(telCtx); err != nil {
		// Bypass the slog OTLP bridge — providers are tearing down.
		fmt.Fprintf(os.Stderr, "otlp shutdown timed out: %v\n", err)
		errs = append(errs, fmt.Errorf("otlp shutdown: %w", err))
	}
	return errors.Join(errs...)
}

// loadAPIKeys returns a populated KeySet (file or CSV) or an empty one when
// neither source is configured. When --api-keys-file is set and the reload
// interval is positive, a background goroutine re-reads the file periodically
// so a Kubernetes Secret rotation is picked up without a restart.
func loadAPIKeys(ctx context.Context, cfg config.Config, logger *slog.Logger) (*auth.KeySet, error) {
	ks := auth.NewKeySet()
	switch {
	case cfg.APIKeysFile != "":
		if err := ks.LoadFile(cfg.APIKeysFile); err != nil {
			return nil, err
		}
		logger.Info("api key auth enabled (file)",
			"path", cfg.APIKeysFile,
			"keys", ks.Snapshot(),
			"reload_interval", cfg.APIKeysReloadInterval,
		)
		if cfg.APIKeysReloadInterval > 0 {
			go reloadAPIKeys(ctx, ks, cfg.APIKeysFile, cfg.APIKeysReloadInterval, logger)
		}
	case cfg.APIKeys != "":
		ks.LoadCSV(cfg.APIKeys)
		logger.Info("api key auth enabled (env)", "keys", ks.Snapshot())
	default:
		logger.Warn("api key auth DISABLED — no --api-keys-file or --api-keys configured")
	}
	return ks, nil
}

func reloadAPIKeys(ctx context.Context, ks *auth.KeySet, path string, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reloadAPIKeysOnce(ks, path, logger)
		}
	}
}

// reloadAPIKeysOnce performs one reload tick. It runs on a bare goroutine
// with no recover above it, so a panic here would kill the whole process —
// recover, log, and let the next tick retry instead.
func reloadAPIKeysOnce(ks *auth.KeySet, path string, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("api keys reload panicked",
				"path", path,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
		}
	}()
	// ReloadFile fails closed: an empty/comment-only/truncated file cannot
	// wipe a non-empty active set — the error below then surfaces every
	// interval until the file is fixed. The changed flag is set-based, so the
	// common same-count rotation (N old keys → N new keys) still logs.
	changed, err := ks.ReloadFile(path)
	if err != nil {
		logger.Error("api keys reload failed", "path", path, "err", err)
		return
	}
	if changed {
		logger.Info("api keys reloaded", "path", path, "keys", ks.Snapshot())
	}
}
