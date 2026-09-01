package build

import (
	"time"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

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
	// LabelKeys names the upstream labels the request's `az` / `env` filter
	// dimensions are matched against. The zero value means the defaults
	// (`az`, `env`); validation lives where the keys are configured.
	LabelKeys promql.LabelKeys
	// VolumeKey derives a claim's bound PersistentVolume name into the token
	// matched against the STOCK Harvest `volume` label (the ONTAP FlexVol
	// name), and decides how that token is compared. Nil (the zero value)
	// means the defaults: replace `-` with `_`, match as a suffix.
	//
	// It is a pre-compiled value rather than raw rules so an invalid regular
	// expression surfaces at NewVolumeKeyRewriter — the caller's own call site,
	// where it can be reported as a configuration error — and never inside a
	// build, where the only honest options would be a silent fallback to a
	// different estate's naming or a panic.
	VolumeKey *VolumeKeyRewriter
	// QoSScopeBatchBytes bounds the rendered `volume` alternation of one
	// scoped QoS workload query, so a large estate is split across several
	// queries rather than exceeding the upstream's query-length limit. Zero
	// (the default) means DefaultQoSScopeBatchBytes.
	QoSScopeBatchBytes int
}
