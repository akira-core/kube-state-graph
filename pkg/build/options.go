package build

import "time"

// Options configures a Builder. It carries only the build-relevant settings,
// decoupled from any server-side configuration struct, so the package is
// importable by other modules without dragging in internal/config.
type Options struct {
	// APITimeout bounds the cheap up{} probe used for the outside-retention
	// check. Zero means the probe inherits the caller's context deadline.
	APITimeout time.Duration
	// RouteResolver resolves global/ingress FQDN peers of server="unknown"
	// service-graph endpoints to in-cluster Services via versioned Istio
	// config (see routeresolve.go). Nil (the default) disables the feature:
	// behaviour is byte-for-byte identical to a build without it.
	RouteResolver RouteResolver
	// RouteResolveTimeout bounds each individual RouteResolver call made
	// during the pre-parse resolution pass. Zero means each call inherits
	// only the build context's deadline.
	RouteResolveTimeout time.Duration
}
