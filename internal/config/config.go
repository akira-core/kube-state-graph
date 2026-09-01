package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

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
	// RouteStoreDSN is the ClickHouse DSN of the versioned Istio-config store
	// backing global-FQDN route resolution (translate-global-fqdn-to-k8s-service).
	// Empty (the default) disables the feature entirely: no store is dialed, no
	// RouteResolver is constructed, and the service-graph reader behaves
	// byte-for-byte as it did before route resolution existed.
	RouteStoreDSN string
	// RouterCheckBin is the path to the native Envoy router_check_tool binary
	// the route engine execs for route matching. Verified executable at
	// startup when RouteStoreDSN is set; ignored otherwise.
	RouterCheckBin string
	// RouteResolveTimeout bounds each individual route-engine call made during
	// a build's pre-parse resolution pass. Zero means each call inherits only
	// the build deadline (--build-timeout).
	RouteResolveTimeout time.Duration
	// RouteStoreUniqueRows enables the route store's pruned read mode: the
	// valid_to predicate is pushed back into SQL, so closed versions are
	// skipped server-side. ONLY safe when the exporter writes one physical
	// row per version (closeMode=update, after historical convergence);
	// enabling it against the default rewrite-close exporter resurrects
	// stale open rows — see pkg/route/store's CH doc. Default false.
	RouteStoreUniqueRows bool
	// RouteStoreUsername / RouteStorePassword are optional ClickHouse native
	// auth credentials for the route store. Env-only (KSG_ROUTE_STORE_USERNAME /
	// KSG_ROUTE_STORE_PASSWORD) — deliberately NO CLI flags, since
	// credential-carrying flags leak through process listings and container
	// specs. Both must be set together or both left empty; Validate rejects a
	// half-configured pair. When set, they override any userinfo embedded in
	// RouteStoreDSN at dial time. Rotation requires a restart (no hot reload).
	RouteStoreUsername string
	RouteStorePassword string
	// PromUsername / PromPassword are optional HTTP Basic Auth credentials for
	// the upstream VictoriaMetrics endpoint. Env-only (KSG_PROM_USERNAME /
	// KSG_PROM_PASSWORD) — deliberately NO CLI flags, since credential-carrying
	// flags leak through process listings and container specs. Both must be set
	// together or both left empty; Validate rejects a half-configured pair.
	// Rotation requires a restart (no hot reload). See
	// openspec/changes/add-prom-basic-auth/design.md D-A1/D-A2.
	PromUsername string
	PromPassword string
	// AZLabel / EnvLabel name the UPSTREAM labels the `az` and `env` request
	// parameters are matched against (KSG_AZ_LABEL / KSG_ENV_LABEL,
	// --az-label / --env-label). The request parameter names themselves are
	// fixed — only the label binding moves, so a deployment whose scrape
	// config stamps e.g. `topology_zone` configures it here rather than
	// asking clients to rename their queries. Validated as PromQL label names
	// and required to differ.
	AZLabel  string
	EnvLabel string
	// NetAppVolumeKeyRewrite is the ordered list of `<pattern>=<replacement>`
	// rewrite rules deriving the Harvest match token from a PVC's bound
	// PersistentVolume name (--netapp-volume-key-rewrite, repeatable;
	// KSG_NETAPP_VOLUME_KEY_REWRITE, semicolon-separated). Each entry splits on
	// its FIRST `=`; a pattern needing a literal `=` writes `\x3d`. Nil (the
	// default) adopts build.DefaultVolumeKeyRules — replace `-` with `_`, the
	// transformation a CSI provisioner performs to make a `pvc-<uuid>` PV name
	// a legal ONTAP volume name. An explicitly empty value is an identity
	// rewrite.
	NetAppVolumeKeyRewrite []string
	// NetAppVolumeMatchMode decides how that token is compared against the
	// stock Harvest `volume` label: exact, suffix, contains or regex
	// (--netapp-volume-match-mode / KSG_NETAPP_VOLUME_MATCH_MODE). Defaults to
	// suffix, which resolves a FlexVol without the deployment declaring the
	// provisioner's storage prefix.
	NetAppVolumeMatchMode string
	// NetAppQoSScopeBatchBytes bounds the rendered `volume` alternation of one
	// scoped QoS workload query (--netapp-qos-scope-batch-bytes /
	// KSG_NETAPP_QOS_SCOPE_BATCH_BYTES), so a large estate is split across
	// several queries instead of exceeding the upstream's query-length limit.
	NetAppQoSScopeBatchBytes int
	// BackendsFile is the path to the mounted routing table declaring the
	// upstream backends queries are dispatched across (--backends-file /
	// KSG_BACKENDS_FILE). Empty (the default) synthesises a single implicit
	// backend addressed at PromURL, serving every family with no zones, so a
	// deployment that configures nothing new behaves exactly as it did before
	// backend routing existed.
	//
	// When set, it takes precedence over PromURL, which is then ignored.
	BackendsFile string
	// BackendsReloadInterval is how often the routing table file is re-read.
	// Zero disables reloading: the table read at startup serves for the
	// process lifetime. Mirrors APIKeysReloadInterval, the existing
	// mounted-file hot-reload precedent.
	BackendsReloadInterval time.Duration
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
		RouteStoreDSN:         "",
		RouterCheckBin:        "/usr/local/bin/router_check_tool",
		RouteResolveTimeout:   5 * time.Second,
		RouteStoreUniqueRows:  false,
		RouteStoreUsername:    "",
		RouteStorePassword:    "",
		PromUsername:          "",
		PromPassword:          "",
		AZLabel:               promql.DefaultAZLabel,
		EnvLabel:              promql.DefaultEnvLabel,
		// Nil, not an empty slice: nil means "the operator configured none"
		// and adopts build.DefaultVolumeKeyRules, while an explicitly empty
		// list is an identity rewrite.
		NetAppVolumeKeyRewrite:   nil,
		NetAppVolumeMatchMode:    string(build.DefaultVolumeMatchMode),
		NetAppQoSScopeBatchBytes: build.DefaultQoSScopeBatchBytes,
		BackendsFile:             "",
		// Matches APIKeysReloadInterval: the same mounted-file cadence, so an
		// operator reasons about one reload period, not two.
		BackendsReloadInterval: 30 * time.Second,
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
	fs.DurationVar(&cfg.APITimeout, "api-timeout", cfg.APITimeout, "Per-request context timeout for upstream calls outside a graph build (/readyz probe, outside-retention probe).")
	fs.StringVar(&cfg.APIKeysFile, "api-keys-file", cfg.APIKeysFile, "Path to a file holding accepted API keys (one per line, # comments allowed). Reloaded periodically. Takes precedence over --api-keys.")
	fs.StringVar(&cfg.APIKeys, "api-keys", cfg.APIKeys, "Comma-separated list of accepted API keys. Used when --api-keys-file is unset.")
	fs.DurationVar(&cfg.APIKeysReloadInterval, "api-keys-reload-interval", cfg.APIKeysReloadInterval, "How often to re-read --api-keys-file. Set to 0 to disable hot reload.")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level: debug, info, warn, error.")
	fs.StringVar(&cfg.BackendsFile, "backends-file", cfg.BackendsFile, "Path to the routing table (YAML or JSON) declaring the upstream backends queries are dispatched across. Unset uses a single implicit backend at --prom-url.")
	fs.DurationVar(&cfg.BackendsReloadInterval, "backends-reload-interval", cfg.BackendsReloadInterval, "How often to re-read --backends-file. Set to 0 to disable hot reload.")
	fs.StringVar(&cfg.AZLabel, "az-label", cfg.AZLabel, "Upstream label the ?az= request parameter is matched against on every topology query.")
	fs.StringVar(&cfg.EnvLabel, "env-label", cfg.EnvLabel, "Upstream label the ?env= request parameter is matched against on every topology query.")
	fs.Var(&volumeKeyRewriteFlag{dst: &cfg.NetAppVolumeKeyRewrite}, "netapp-volume-key-rewrite", "Ordered `<regex>=<replacement>` rule deriving the Harvest match token from a PVC's bound PV name. Repeat for several rules, applied in order. Unset uses `-=_`. Splits on the first `=`; write \\x3d for a literal one.")
	fs.StringVar(&cfg.NetAppVolumeMatchMode, "netapp-volume-match-mode", cfg.NetAppVolumeMatchMode, "How the derived token is matched against the stock Harvest `volume` label: exact, suffix, contains or regex.")
	fs.IntVar(&cfg.NetAppQoSScopeBatchBytes, "netapp-qos-scope-batch-bytes", cfg.NetAppQoSScopeBatchBytes, "Byte budget for one scoped QoS query's `volume` alternation. A larger matched set is split across several queries.")
	fs.StringVar(&cfg.RouteStoreDSN, "route-store-dsn", cfg.RouteStoreDSN, "ClickHouse DSN of the versioned Istio-config store for global-FQDN route resolution (e.g. clickhouse://host:9000/routing). Prefer KSG_ROUTE_STORE_USERNAME / KSG_ROUTE_STORE_PASSWORD for credentials. Empty (default) disables route resolution entirely.")
	fs.StringVar(&cfg.RouterCheckBin, "router-check-bin", cfg.RouterCheckBin, "Path to the native Envoy router_check_tool binary used by route resolution. Only consulted when --route-store-dsn is set.")
	fs.DurationVar(&cfg.RouteResolveTimeout, "route-resolve-timeout", cfg.RouteResolveTimeout, "Per-endpoint timeout for each route-engine resolution during a build. 0 inherits the build deadline only.")
	fs.BoolVar(&cfg.RouteStoreUniqueRows, "route-store-unique-rows", cfg.RouteStoreUniqueRows, "Enable the route store's pruned read mode (server-side valid_to filtering). ONLY when the exporter guarantees one physical row per version (closeMode=update); never against the default rewrite-close exporter.")

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
	// getInt mirrors getDur: a non-numeric value fails at startup rather than
	// silently keeping the default.
	getInt := func(env string, dst *int) error {
		if v, ok := lookup(env); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("invalid %s=%q: must be an integer", env, v)
			}
			*dst = n
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
	getStr("KSG_BACKENDS_FILE", &cfg.BackendsFile)
	if err := getDur("KSG_BACKENDS_RELOAD_INTERVAL", &cfg.BackendsReloadInterval); err != nil {
		return err
	}
	getStr("KSG_LOG_LEVEL", &cfg.LogLevel)
	getStr("KSG_AZ_LABEL", &cfg.AZLabel)
	getStr("KSG_ENV_LABEL", &cfg.EnvLabel)
	// Semicolon-separated, not comma-separated: a regular expression may
	// legitimately contain a comma (`a{2,3}`) but a semicolon is far rarer, and
	// the flag form exists for anything the separator cannot express.
	if v, ok := lookup("KSG_NETAPP_VOLUME_KEY_REWRITE"); ok {
		cfg.NetAppVolumeKeyRewrite = splitAndTrimOn(v, ";")
	}
	getStr("KSG_NETAPP_VOLUME_MATCH_MODE", &cfg.NetAppVolumeMatchMode)
	if err := getInt("KSG_NETAPP_QOS_SCOPE_BATCH_BYTES", &cfg.NetAppQoSScopeBatchBytes); err != nil {
		return err
	}
	getStr("KSG_ROUTE_STORE_DSN", &cfg.RouteStoreDSN)
	// Env-only by design — no matching flags are registered in Parse
	// (same rationale as KSG_PROM_USERNAME / KSG_PROM_PASSWORD).
	getStr("KSG_ROUTE_STORE_USERNAME", &cfg.RouteStoreUsername)
	getStr("KSG_ROUTE_STORE_PASSWORD", &cfg.RouteStorePassword)
	getStr("KSG_ROUTER_CHECK_BIN", &cfg.RouterCheckBin)
	if err := getDur("KSG_ROUTE_RESOLVE_TIMEOUT", &cfg.RouteResolveTimeout); err != nil {
		return err
	}
	if v, ok := lookup("KSG_ROUTE_STORE_UNIQUE_ROWS"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid KSG_ROUTE_STORE_UNIQUE_ROWS=%q: must be a boolean", v)
		}
		cfg.RouteStoreUniqueRows = b
	}
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
	if c.BackendsReloadInterval < 0 {
		return errors.New("backends-reload-interval must be >= 0 (0 disables hot reload)")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log-level: %q", c.LogLevel)
	}
	// Route-store credentials must be configured as a pair. The error names
	// the env vars only — never echo the configured values.
	if (c.RouteStoreUsername == "") != (c.RouteStorePassword == "") {
		return errors.New("KSG_ROUTE_STORE_USERNAME and KSG_ROUTE_STORE_PASSWORD must be set together (or both left unset)")
	}
	// Route-resolution knobs are only meaningful with a store DSN; the binary
	// path itself is verified executable at startup (matchcheck.NewRunner), not
	// here — Validate stays filesystem-free.
	if c.RouteStoreDSN != "" && c.RouterCheckBin == "" {
		return errors.New("router-check-bin is required when route-store-dsn is set")
	}
	if c.RouteResolveTimeout < 0 {
		return errors.New("route-resolve-timeout must be >= 0 (0 inherits the build deadline)")
	}
	// The az / env label keys are rendered verbatim into every topology query,
	// so an invalid key would produce a PromQL parse error on every request
	// instead of a startup failure. Reject it here, naming the setting.
	if !labelNameRE.MatchString(c.AZLabel) {
		return fmt.Errorf("az-label (KSG_AZ_LABEL) is not a valid PromQL label name: %q", c.AZLabel)
	}
	if !labelNameRE.MatchString(c.EnvLabel) {
		return fmt.Errorf("env-label (KSG_ENV_LABEL) is not a valid PromQL label name: %q", c.EnvLabel)
	}
	if c.AZLabel == c.EnvLabel {
		return fmt.Errorf("az-label and env-label must differ (both %q): one matcher would overwrite the other", c.AZLabel)
	}
	// The derivation decides which FlexVol every claim joins, so an
	// uncompilable pattern or an unknown mode fails HERE rather than resolving
	// a different estate than the operator declared. Compiling it also proves
	// the value handed to build.Options is usable.
	if _, err := c.VolumeKeyRewriter(); err != nil {
		return fmt.Errorf("netapp volume key derivation is invalid: %w", err)
	}
	if c.NetAppQoSScopeBatchBytes <= 0 {
		return fmt.Errorf(
			"netapp-qos-scope-batch-bytes (KSG_NETAPP_QOS_SCOPE_BATCH_BYTES) must be > 0, got %d",
			c.NetAppQoSScopeBatchBytes)
	}
	return nil
}

// labelNameRE is the PromQL label-name grammar.
var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// volumeKeyRewriteFlag collects repeated --netapp-volume-key-rewrite values in
// declaration order. The FIRST occurrence discards the default (nil) rather
// than appending to it, so a deployment that configures any rule configures the
// whole ordered list — a half-inherited default would apply a rewrite the
// operator never wrote.
type volumeKeyRewriteFlag struct {
	dst  *[]string
	seen bool
}

func (f *volumeKeyRewriteFlag) String() string {
	if f == nil || f.dst == nil {
		return ""
	}
	return strings.Join(*f.dst, ";")
}

func (f *volumeKeyRewriteFlag) Set(v string) error {
	if !f.seen {
		// Discard whatever the default or the environment supplied: the first
		// flag occurrence starts the list. Appending instead would apply an
		// inherited rule the operator never wrote, in a position they never
		// chose — and order is load-bearing here.
		*f.dst = nil
		f.seen = true
	}
	*f.dst = append(*f.dst, v)
	return nil
}

// ParseVolumeKeyRules turns the configured `<pattern>=<replacement>` entries
// into build rules. Splitting on the FIRST `=` keeps a replacement free to
// contain one; a PATTERN needing a literal `=` writes the escape `\x3d`, which
// Go's regexp accepts.
//
// A nil input stays nil — "the operator configured none", which
// build.NewVolumeKeyRewriter resolves to the defaults. A non-nil empty input
// stays a non-nil empty slice: an explicit identity rewrite.
func ParseVolumeKeyRules(entries []string) ([]build.VolumeKeyRule, error) {
	if entries == nil {
		return nil, nil
	}
	out := make([]build.VolumeKeyRule, 0, len(entries))
	for i, e := range entries {
		pattern, replacement, ok := strings.Cut(e, "=")
		if !ok {
			return nil, fmt.Errorf(
				"netapp-volume-key-rewrite rule %d (%q) has no %q separator: write <regex>=<replacement>",
				i+1, e, "=")
		}
		if pattern == "" {
			return nil, fmt.Errorf(
				"netapp-volume-key-rewrite rule %d (%q) has an empty pattern", i+1, e)
		}
		out = append(out, build.VolumeKeyRule{Pattern: pattern, Replacement: replacement})
	}
	return out, nil
}

// VolumeKeyRewriter compiles the configured derivation. Callers construct it
// once at startup and hand the result to build.Options, so an invalid pattern is
// a startup failure rather than a per-build one.
func (c Config) VolumeKeyRewriter() (*build.VolumeKeyRewriter, error) {
	rules, err := ParseVolumeKeyRules(c.NetAppVolumeKeyRewrite)
	if err != nil {
		return nil, err
	}
	return build.NewVolumeKeyRewriter(rules, build.VolumeMatchMode(c.NetAppVolumeMatchMode))
}

func splitAndTrimOn(v, sep string) []string {
	parts := strings.Split(v, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
