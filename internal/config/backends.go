package config

import (
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
)

// The routing file's schema, parse, and credential resolution live in
// pkg/promql/backendsfile so an external module embedding the graph engine
// configures routing with the same code this binary runs — internal/* is
// unreachable from outside the module (design D3 / D14). The functions here
// are delegations kept so the binary's call sites and their tests are
// unchanged.

// ReadBackendsFile reads and parses the routing table at path.
func ReadBackendsFile(path string, lookup LookupEnvFunc) (*promql.Table, error) {
	return backendsfile.Read(path, backendsfile.LookupEnvFunc(lookup))
}

// ParseBackendsFile parses a routing table and resolves each backend's
// credentials from the environment through lookup.
func ParseBackendsFile(data []byte, lookup LookupEnvFunc) (*promql.Table, error) {
	return backendsfile.Parse(data, backendsfile.LookupEnvFunc(lookup))
}

// SingleBackendTable synthesises the implicit routing table a deployment with
// no routing file runs on: one backend named "default", addressed at
// --prom-url, serving EVERY family, with no zones (a catch-all).
//
// It is a table rather than a separate unrouted code path so the compatibility
// claim is exercised by the whole existing test suite: every current unit,
// component, golden, and integration test runs through the router in this
// degenerate configuration.
func SingleBackendTable(promURL, username, password string) (*promql.Table, error) {
	return promql.SingleBackendTable(promURL, username, password)
}

// DefaultBackendName is the name the implicit single-backend table uses. It
// appears in logs, in the per-backend failure metric, and on query spans.
const DefaultBackendName = promql.DefaultBackendName
