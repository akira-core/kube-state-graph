// Package backendsfile reads the upstream routing table from the file an
// operator mounts, and produces the validated promql.Table the router
// dispatches through.
//
// It is a subpackage rather than part of pkg/promql because pkg/promql stays
// free of file I/O and of any parser dependency: a module that builds its table
// in code imports pkg/promql alone and inherits neither. It is a pkg/
// subpackage rather than internal/config because the graph engine is an
// importable library — an external module cannot import internal/*, and a
// parser there would force every embedder to hand-roll the same schema, the
// same validation and the same credential rules, three copies of a contract
// whose failure mode is a silently mis-routed family (design D3 / D14).
package backendsfile

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// LookupEnvFunc matches os.LookupEnv's signature so a caller can inject
// credential values instead of touching the process environment.
type LookupEnvFunc func(string) (string, bool)

// backendsFile is the on-disk schema of the routing table.
//
// It is parsed with sigs.k8s.io/yaml, which converts YAML to JSON and then uses
// encoding/json — so one struct with json tags accepts BOTH forms. A routing
// table lives in a ConfigMap, which operators write as YAML; requiring JSON
// inside it would be a papercut on day one.
type backendsFile struct {
	Backends []backendEntry `json:"backends"`
}

type backendEntry struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Families []string `json:"families"`
	// Zones is optional. Omitted or empty means EVERY zone — a catch-all
	// backend.
	Zones []string `json:"zones,omitempty"`
	// UsernameEnv / PasswordEnv name the environment variables holding this
	// backend's basic-auth pair. They are NAMES, never values: the routing
	// file is a ConfigMap and must never hold a secret.
	UsernameEnv string `json:"usernameEnv,omitempty"`
	PasswordEnv string `json:"passwordEnv,omitempty"`
	// Username / Password exist ONLY so a file carrying them can be rejected
	// with an error that explains where credentials belong. Strict parsing
	// would otherwise reject them as unknown fields, which says nothing useful.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Read reads and parses the routing table at path. A nil lookup reads the
// process environment.
func Read(path string, lookup LookupEnvFunc) (*promql.Table, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("backends file %s: %w", path, err)
	}
	return Parse(data, lookup)
}

// Parse parses a routing table and resolves each backend's credentials from the
// environment through lookup. A nil lookup reads the process environment.
//
// Parsing is STRICT: an unknown field is an error rather than a silent
// no-op, because a misspelled `zone:` for `zones:` would otherwise turn a
// zone-scoped backend into a catch-all with no signal.
//
// Errors name the offending backend and, for credentials, the variable —
// never a credential value.
func Parse(data []byte, lookup LookupEnvFunc) (*promql.Table, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var f backendsFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("backends file: %w", err)
	}
	if len(f.Backends) == 0 {
		return nil, fmt.Errorf("backends file: declares no backends — at least one is required")
	}

	// The global pair is the fallback for a backend naming no variables of its
	// own. It is read through the same lookup so a test can inject it.
	globalUser, _ := lookup(GlobalUsernameEnv)
	globalPass, _ := lookup(GlobalPasswordEnv)

	out := make([]promql.Backend, 0, len(f.Backends))
	for _, e := range f.Backends {
		if e.Username != "" || e.Password != "" {
			return nil, fmt.Errorf(
				"backend %q: credentials must not appear in the routing file — name the environment variables holding them with usernameEnv / passwordEnv",
				e.Name)
		}
		families := make([]promql.Family, 0, len(e.Families))
		for _, raw := range e.Families {
			fam, ok := promql.ParseFamily(raw)
			if !ok {
				return nil, fmt.Errorf("backend %q: unknown family %q", e.Name, raw)
			}
			families = append(families, fam)
		}
		user, pass, err := resolveBackendCredentials(e, lookup, globalUser, globalPass)
		if err != nil {
			return nil, err
		}
		out = append(out, promql.NewBackend(e.Name, e.URL, families, e.Zones, user, pass))
	}

	// Every structural rule — unique names, parseable URLs, non-empty family
	// sets, full family coverage — lives in NewTable, so the file path and an
	// embedder constructing a table directly are validated identically.
	return promql.NewTable(out)
}

// GlobalUsernameEnv / GlobalPasswordEnv name the process-wide basic-auth pair a
// backend declaring no variables of its own falls back to.
const (
	GlobalUsernameEnv = "KSG_PROM_USERNAME"
	GlobalPasswordEnv = "KSG_PROM_PASSWORD" //nolint:gosec // an environment variable NAME, never a credential value
)

// resolveBackendCredentials reads a backend's basic-auth pair from the
// environment.
//
// A backend naming exactly one variable is rejected: the half-configured pair
// is always a mistake, and guessing which half was meant would authenticate
// with an empty username or password.
//
// A named variable that is unset or empty is also rejected, rather than
// falling back to the global pair or to no credentials. A quiet fallback turns
// a typo'd variable name into 401s from one store — which, since a backend
// failure fails the whole query, surfaces as an error pointing at the wrong
// thing.
func resolveBackendCredentials(e backendEntry, lookup LookupEnvFunc, globalUser, globalPass string) (string, string, error) {
	switch {
	case e.UsernameEnv == "" && e.PasswordEnv == "":
		return globalUser, globalPass, nil
	case e.UsernameEnv == "" || e.PasswordEnv == "":
		return "", "", fmt.Errorf(
			"backend %q: usernameEnv and passwordEnv must be set together or both omitted (got usernameEnv=%q, passwordEnv=%q)",
			e.Name, e.UsernameEnv, e.PasswordEnv)
	}

	user, ok := lookup(e.UsernameEnv)
	if !ok || user == "" {
		return "", "", fmt.Errorf("backend %q: environment variable %s (usernameEnv) is unset or empty", e.Name, e.UsernameEnv)
	}
	pass, ok := lookup(e.PasswordEnv)
	if !ok || pass == "" {
		return "", "", fmt.Errorf("backend %q: environment variable %s (passwordEnv) is unset or empty", e.Name, e.PasswordEnv)
	}
	return user, pass, nil
}
