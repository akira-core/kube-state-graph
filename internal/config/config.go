package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// metricPrefixPattern enforces the Prometheus metric-name charset
// (https://prometheus.io/docs/concepts/data_model/#metric-names-and-labels).
// Empty MetricPrefix is allowed and bypasses this check.
var metricPrefixPattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Config holds the parsed runtime configuration for the kube-state-graph server.
type Config struct {
	PromURL               string
	ListenAddr            string
	BuildTimeout          time.Duration
	APITimeout            time.Duration
	APIKeysFile           string
	APIKeys               string
	APIKeysReloadInterval time.Duration
	LogLevel              string
	// MetricPrefix is prepended verbatim to every kube-state-metrics-shaped
	// series name the topology reader queries (and to the cluster-discovery
	// query). Empty (the default) preserves stock kube-state-metrics behaviour.
	// See design.md D26.
	MetricPrefix string
	// PromUsername / PromPassword are optional HTTP Basic Auth credentials for
	// the upstream VictoriaMetrics endpoint. Env-only (KSG_PROM_USERNAME /
	// KSG_PROM_PASSWORD) — deliberately NO CLI flags, since credential-carrying
	// flags leak through process listings and container specs. Both must be set
	// together or both left empty; Validate rejects a half-configured pair.
	// Rotation requires a restart (no hot reload). See
	// openspec/changes/add-prom-basic-auth/design.md D-A1/D-A2.
	PromUsername string
	PromPassword string
}

// LookupEnvFunc matches os.LookupEnv signature so tests can inject env values.
type LookupEnvFunc func(string) (string, bool)

// Defaults returns a Config populated with the documented v1 defaults.
func Defaults() Config {
	return Config{
		PromURL:               "http://localhost:8428",
		ListenAddr:            ":8080",
		BuildTimeout:          15 * time.Second,
		APITimeout:            5 * time.Second,
		APIKeysFile:           "",
		APIKeys:               "",
		APIKeysReloadInterval: 30 * time.Second,
		LogLevel:              "info",
		MetricPrefix:          "",
		PromUsername:          "",
		PromPassword:          "",
	}
}

// Parse parses CLI args + env vars into a Config and validates it.
// Env vars override defaults; flags override env vars.
func Parse(args []string, lookup LookupEnvFunc) (Config, error) {
	cfg := Defaults()
	if err := applyEnv(&cfg, lookup); err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("kube-state-graph", flag.ContinueOnError)
	fs.StringVar(&cfg.PromURL, "prom-url", cfg.PromURL, "VictoriaMetrics Prometheus-compatible URL.")
	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "HTTP listen address.")
	fs.DurationVar(&cfg.BuildTimeout, "build-timeout", cfg.BuildTimeout, "Per-build context timeout for /v1/graph.")
	fs.DurationVar(&cfg.APITimeout, "api-timeout", cfg.APITimeout, "Per-request context timeout for non-graph endpoints with upstream calls (/v1/clusters, /readyz).")
	fs.StringVar(&cfg.APIKeysFile, "api-keys-file", cfg.APIKeysFile, "Path to a file holding accepted API keys (one per line, # comments allowed). Reloaded periodically. Takes precedence over --api-keys.")
	fs.StringVar(&cfg.APIKeys, "api-keys", cfg.APIKeys, "Comma-separated list of accepted API keys. Used when --api-keys-file is unset.")
	fs.DurationVar(&cfg.APIKeysReloadInterval, "api-keys-reload-interval", cfg.APIKeysReloadInterval, "How often to re-read --api-keys-file. Set to 0 to disable hot reload.")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level: debug, info, warn, error.")
	fs.StringVar(&cfg.MetricPrefix, "metric-prefix", cfg.MetricPrefix, "Additive prefix prepended to every kube-state-metrics-shaped series name the topology reader queries (e.g. \"o11y_\" → o11y_kube_pod_info). Empty (default) preserves stock kube-state-metrics behaviour. Trailing underscore is the operator's responsibility — none is injected. Does not affect traces_service_graph_request_total or up{}.")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config, lookup LookupEnvFunc) error {
	getStr := func(env string, dst *string) {
		if v, ok := lookup(env); ok {
			*dst = v
		}
	}
	// getDur surfaces a parse error instead of silently keeping the default, so
	// a misconfigured duration env (e.g. KSG_BUILD_TIMEOUT=15 with no unit)
	// fails loudly at startup — parity with the flag path (flag.DurationVar).
	getDur := func(env string, dst *time.Duration) error {
		if v, ok := lookup(env); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("invalid %s=%q: must be a Go duration such as 15s or 2m", env, v)
			}
			*dst = d
		}
		return nil
	}

	getStr("KSG_PROM_URL", &cfg.PromURL)
	// Env-only by design — no matching flags are registered in Parse (D-A1).
	getStr("KSG_PROM_USERNAME", &cfg.PromUsername)
	getStr("KSG_PROM_PASSWORD", &cfg.PromPassword)
	getStr("KSG_LISTEN_ADDR", &cfg.ListenAddr)
	if err := getDur("KSG_BUILD_TIMEOUT", &cfg.BuildTimeout); err != nil {
		return err
	}
	if err := getDur("KSG_API_TIMEOUT", &cfg.APITimeout); err != nil {
		return err
	}
	getStr("KSG_API_KEYS_FILE", &cfg.APIKeysFile)
	getStr("KSG_API_KEYS", &cfg.APIKeys)
	if err := getDur("KSG_API_KEYS_RELOAD_INTERVAL", &cfg.APIKeysReloadInterval); err != nil {
		return err
	}
	getStr("KSG_LOG_LEVEL", &cfg.LogLevel)
	getStr("KSG_METRIC_PREFIX", &cfg.MetricPrefix)
	return nil
}

// Validate checks Config invariants.
func (c Config) Validate() error {
	if c.PromURL == "" {
		return errors.New("prom-url is required")
	}
	u, err := url.Parse(c.PromURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("prom-url is not a valid URL: %q", c.PromURL)
	}
	// Upstream basic-auth credentials must be configured as a pair. The error
	// names the env vars only — never echo the configured values (D-A2/D-A5).
	if (c.PromUsername == "") != (c.PromPassword == "") {
		return errors.New("KSG_PROM_USERNAME and KSG_PROM_PASSWORD must be set together (or both left unset)")
	}
	// RFC 7617 forbids ':' in basic-auth user-ids: SetBasicAuth encodes
	// user+":"+pass, so a colon silently shifts everything after it into the
	// password and every upstream request 401s with no client-visible hint
	// (the raw 401 detail is redacted to server logs). Fail fast at startup.
	if strings.Contains(c.PromUsername, ":") {
		return errors.New("KSG_PROM_USERNAME must not contain ':' (RFC 7617 basic-auth user-id)")
	}
	if c.ListenAddr == "" {
		return errors.New("listen-addr is required")
	}
	if c.BuildTimeout <= 0 {
		return errors.New("build-timeout must be positive")
	}
	if c.APITimeout <= 0 {
		return errors.New("api-timeout must be positive")
	}
	if c.APIKeysReloadInterval < 0 {
		return errors.New("api-keys-reload-interval must be >= 0 (0 disables hot reload)")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log-level: %q", c.LogLevel)
	}
	if c.MetricPrefix != "" && !metricPrefixPattern.MatchString(c.MetricPrefix) {
		return fmt.Errorf("invalid metric-prefix %q: must match %s", c.MetricPrefix, metricPrefixPattern)
	}
	return nil
}

func splitAndTrim(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
