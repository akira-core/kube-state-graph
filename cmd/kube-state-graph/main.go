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

	"github.com/akira-core/kube-state-graph/internal/api"
	"github.com/akira-core/kube-state-graph/internal/auth"
	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/internal/observability"
	"github.com/akira-core/kube-state-graph/internal/telemetry"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/akira-core/kube-state-graph/pkg/route"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
	routestore "github.com/akira-core/kube-state-graph/pkg/route/store"
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
		"otlp_enabled", telemetryProviders.Enabled,
		// Boolean only — the credential values themselves are never logged.
		"prom_basic_auth", cfg.PromUsername != "",
		"backends_file", cfg.BackendsFile,
		"route_store_auth", cfg.RouteStoreUsername != "",
	)

	metrics := observability.NewMetrics()
	// Every upstream call is dispatched through the router, including the
	// single-backend case: with no --backends-file an implicit `default`
	// backend at --prom-url serves every family, so the compatibility claim is
	// exercised by the whole existing test suite rather than by a branch.
	//
	// Upstream basic auth is env-only (KSG_PROM_USERNAME / KSG_PROM_PASSWORD,
	// or a backend's own usernameEnv / passwordEnv); config.Validate
	// guarantees the global pair is set together or not at all. Neither the
	// startup log nor the routing table's rendered form carries a value.
	promRouter, err := buildRouter(cfg, metrics, logger, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("upstream backends: %w", err)
	}
	startBackendReload(appCtx, promRouter, cfg, logger, metrics, os.LookupEnv)

	keys, err := loadAPIKeys(appCtx, cfg, logger)
	if err != nil {
		return fmt.Errorf("api keys: %w", err)
	}

	// Route resolution (translate-global-fqdn-to-k8s-service) is opt-in: an
	// empty DSN leaves routeResolver nil and the service-graph reader behaves
	// exactly as before. When set, both the store schema and the
	// router_check_tool binary are verified here — fail fast at startup, not
	// on the first build that needs them. The store connection lives for the
	// process lifetime and is closed on shutdown.
	var routeResolver build.RouteResolver
	if cfg.RouteStoreDSN != "" {
		runner, err := matchcheck.NewRunner(cfg.RouterCheckBin)
		if err != nil {
			return fmt.Errorf("route resolution: %w", err)
		}
		var storeOpts []routestore.Option
		if cfg.RouteStoreUniqueRows {
			storeOpts = append(storeOpts, routestore.WithUniqueRows())
		}
		// Route-store auth is env-only (KSG_ROUTE_STORE_USERNAME /
		// KSG_ROUTE_STORE_PASSWORD); config.Validate guarantees the pair is
		// set together or not at all. Credential values are never logged.
		if cfg.RouteStoreUsername != "" {
			storeOpts = append(storeOpts, routestore.WithAuth(cfg.RouteStoreUsername, cfg.RouteStorePassword))
		}
		storeCtx, storeCancel := context.WithTimeout(appCtx, 10*time.Second)
		routeStore, err := routestore.Open(storeCtx, cfg.RouteStoreDSN, storeOpts...)
		storeCancel()
		if err != nil {
			return fmt.Errorf("route resolution: %w", err)
		}
		defer func() { _ = routeStore.Close() }()
		routeResolver = route.NewResolver(routeStore, runner)
		logger.Info("route resolution enabled",
			"router_check_bin", cfg.RouterCheckBin,
			"route_resolve_timeout", cfg.RouteResolveTimeout,
			"route_store_unique_rows", cfg.RouteStoreUniqueRows,
			"route_store_auth", cfg.RouteStoreUsername != "",
		)
	}

	// promRouter satisfies promql.QuerierSource as well as promql.Querier, so
	// build.New upgrades it and resolves a per-request querier from the live
	// routing table.
	// Already proven to compile by cfg.Validate(); the error is re-checked
	// rather than discarded so a future validation change cannot silently
	// hand build.Options a nil rewriter and fall back to a different estate's
	// naming.
	volumeKey, err := cfg.VolumeKeyRewriter()
	if err != nil {
		return fmt.Errorf("netapp volume key derivation: %w", err)
	}
	builder := build.New(promRouter, build.Options{
		APITimeout:          cfg.APITimeout,
		RouteResolver:       routeResolver,
		RouteResolveTimeout: cfg.RouteResolveTimeout,
		LabelKeys:           promql.LabelKeys{AZ: cfg.AZLabel, Env: cfg.EnvLabel},
		VolumeKey:           volumeKey,
		QoSScopeBatchBytes:  cfg.NetAppQoSScopeBatchBytes,
	}, metrics, nil)
	server := api.New(cfg, builder, promRouter, metrics, logger, keys, nil)

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
