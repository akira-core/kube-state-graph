package main

import (
	"log/slog"
	"net/url"

	"github.com/akira-core/kube-state-graph/internal/config"
)

// warnArgvSecrets logs one Warn per credential the operator supplied where it
// is visible beyond the process: API keys as a literal --api-keys flag (the
// process listing, the container spec, and — before the resource was trimmed
// — every OTLP record), and userinfo embedded in the route-store DSN. Both are
// documented, still-supported inputs; this only tells the operator, it never
// refuses. Neither Warn carries a key, the DSN, or a DSN parse error (a
// *url.Error quotes its whole input).
func warnArgvSecrets(cfg config.Config, logger *slog.Logger) {
	// --api-keys-file takes precedence, so a flag beside it is unused — but
	// still on the command line, so it still warrants the Warn.
	if cfg.APIKeysFromFlag && cfg.APIKeys != "" {
		logger.Warn("api keys passed as a literal --api-keys flag are visible in the process command line; "+
			"use --api-keys-file (a mounted Secret) or KSG_API_KEYS instead",
			"api_keys_file_set", cfg.APIKeysFile != "",
		)
	}
	if dsnHasUserinfo(cfg.RouteStoreDSN) {
		logger.Warn("route-store DSN embeds credentials; "+
			"set KSG_ROUTE_STORE_USERNAME / KSG_ROUTE_STORE_PASSWORD and drop the userinfo from the DSN",
			"route_store_env_auth", cfg.RouteStoreUsername != "",
		)
	}
}

// dsnHasUserinfo reports whether a non-empty DSN parses with userinfo. An
// unparseable DSN reports false: it fails at dial, and this path must not
// surface the parse error.
func dsnHasUserinfo(dsn string) bool {
	if dsn == "" {
		return false
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return false
	}
	_, hasPassword := u.User.Password()
	return hasPassword || u.User.Username() != ""
}
