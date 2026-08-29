package promql

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Backend is one upstream Prometheus-compatible installation the router may
// dispatch a query to. Values are immutable once inside a Table: the accessors
// return copies of the slice fields so a holder cannot mutate a live table.
//
// Username / Password are RESOLVED credential values, not variable names. The
// routing file never carries a value — internal/config reads the named
// environment variables and fills these in before constructing the Table (see
// the design's D8). They are deliberately unexported so no formatting verb,
// log helper, or reflective encoder can reach them.
type Backend struct {
	name     string
	url      string
	families []Family
	zones    []string
	username string
	password string
}

// NewBackend assembles a Backend. Validation happens in NewTable, which is the
// only thing that can produce a routable set — a lone Backend is inert.
func NewBackend(name, rawURL string, families []Family, zones []string, username, password string) Backend {
	return Backend{
		name:     name,
		url:      rawURL,
		families: append([]Family(nil), families...),
		zones:    append([]string(nil), zones...),
		username: username,
		password: password,
	}
}

// Name is the backend's identity in logs, metrics, span attributes, and every
// ordering rule the merge depends on.
func (b Backend) Name() string { return b.name }

// URL is the backend's query endpoint.
func (b Backend) URL() string { return b.url }

// Families returns the families this backend serves.
func (b Backend) Families() []Family { return append([]Family(nil), b.families...) }

// Zones returns the availability zones this backend holds. An empty result
// means EVERY zone — a catch-all backend.
func (b Backend) Zones() []string { return append([]string(nil), b.zones...) }

// Credentials returns the resolved basic-auth pair. Both empty means the
// backend issues unauthenticated requests.
func (b Backend) Credentials() (username, password string) { return b.username, b.password }

// serves reports whether this backend answers queries of family f.
func (b Backend) serves(f Family) bool {
	for _, have := range b.families {
		if have == f {
			return true
		}
	}
	return false
}

// coversZone reports whether this backend holds series for at least one of the
// requested zones. A backend declaring no zones is a catch-all and covers
// every request, including one that names no zone at all.
func (b Backend) coversZone(az []string) bool {
	if len(b.zones) == 0 {
		return true
	}
	for _, want := range az {
		if want == "" {
			continue
		}
		for _, have := range b.zones {
			if have == want {
				return true
			}
		}
	}
	return false
}

// String renders the backend for logs. It names the backend, its endpoint, its
// families and its zones — and NEVER a credential value. It reports only
// WHETHER credentials are configured, mirroring the startup log's
// `prom_basic_auth` boolean.
func (b Backend) String() string {
	return fmt.Sprintf("%s{url=%s families=%s zones=%s auth=%t}",
		b.name, b.url, joinFamilies(b.families), strings.Join(b.zones, ","),
		b.username != "" || b.password != "")
}

func joinFamilies(fs []Family) string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f)
	}
	return strings.Join(out, ",")
}

// Table is an immutable, validated routing table. It is the only thing the
// router dispatches through, and it can be produced only by NewTable, so an
// unvalidated table is unrepresentable.
//
// Backends are held sorted by name. That order IS the merge order (design D5):
// the fan-out concatenates each backend's result in ascending name order and
// keeps the first copy of any duplicated series, which makes the merged result
// a pure function of the value sets rather than of response arrival order.
type Table struct {
	backends []Backend
}

// NewTable validates a backend set and returns the immutable table it forms.
//
// Every rule here is a configuration error the operator can fix, so each error
// names the offending backend or family. None of them can echo a credential
// value: the only credential-shaped inputs are already-resolved values that no
// rule inspects.
func NewTable(backends []Backend) (*Table, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("backend table: no backends declared — at least one is required")
	}

	seenName := make(map[string]struct{}, len(backends))
	served := make(map[Family]struct{}, len(Families))
	out := make([]Backend, 0, len(backends))

	for _, b := range backends {
		if strings.TrimSpace(b.name) == "" {
			return nil, fmt.Errorf("backend table: a backend declares an empty name")
		}
		if _, dup := seenName[b.name]; dup {
			return nil, fmt.Errorf("backend %q: duplicate backend name", b.name)
		}
		seenName[b.name] = struct{}{}

		if err := validateBackendURL(b.name, b.url); err != nil {
			return nil, err
		}
		if len(b.families) == 0 {
			return nil, fmt.Errorf("backend %q: declares no families — a backend that serves nothing cannot be routed to", b.name)
		}
		seenFamily := make(map[Family]struct{}, len(b.families))
		for _, f := range b.families {
			if _, ok := ParseFamily(string(f)); !ok {
				return nil, fmt.Errorf("backend %q: unknown family %q (known: %s)", b.name, f, joinFamilies(Families))
			}
			if _, dup := seenFamily[f]; dup {
				return nil, fmt.Errorf("backend %q: family %q declared twice", b.name, f)
			}
			seenFamily[f] = struct{}{}
			served[f] = struct{}{}
		}
		for _, z := range b.zones {
			if strings.TrimSpace(z) == "" {
				return nil, fmt.Errorf("backend %q: declares an empty zone value — omit the zones field for a catch-all backend", b.name)
			}
		}

		out = append(out, normaliseBackend(b))
	}

	// A family served by no backend is fatal, not a degrade: its queries would
	// have nowhere to go, and the resulting empty vector is indistinguishable
	// from a genuinely empty estate.
	for _, f := range Families {
		if _, ok := served[f]; !ok {
			return nil, fmt.Errorf("backend table: family %q is served by no backend — every family must have at least one", f)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return &Table{backends: out}, nil
}

func validateBackendURL(name, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("backend %q: declares an empty url", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("backend %q: url is not parseable: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("backend %q: url scheme %q is not http or https", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("backend %q: url %q has no host", name, raw)
	}
	return nil
}

// normaliseBackend sorts and de-duplicates the set-valued fields so two
// tables differing only in declaration order are indistinguishable — the same
// property Selector.render depends on for query determinism.
func normaliseBackend(b Backend) Backend {
	fams := append([]Family(nil), b.families...)
	sort.Slice(fams, func(i, j int) bool { return fams[i] < fams[j] })
	b.families = fams

	seen := make(map[string]struct{}, len(b.zones))
	zones := make([]string, 0, len(b.zones))
	for _, z := range b.zones {
		if _, dup := seen[z]; dup {
			continue
		}
		seen[z] = struct{}{}
		zones = append(zones, z)
	}
	sort.Strings(zones)
	b.zones = zones
	return b
}

// Backends returns the table's backends in ascending name order — the order
// the fan-out merge depends on.
func (t *Table) Backends() []Backend {
	if t == nil {
		return nil
	}
	return append([]Backend(nil), t.backends...)
}

// Len reports how many backends the table holds. It is what the
// kube_state_graph_upstream_backends gauge reads.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.backends)
}

// Select returns the backends a query of family f must be issued to under a
// request whose `az` dimension carries the value set az, in ascending name
// order.
//
// The rule is the design's D4:
//
//  1. Candidates serve the family.
//  2. When the family accepts the `az` dimension, candidates are further
//     restricted to those covering at least one requested zone (a backend
//     declaring no zones is a catch-all and always covers). An empty az set
//     applies no restriction — every candidate is selected.
//  3. When the family accepts NO request dimension (service graph, probe), the
//     zones field is ignored entirely. Narrowing those families by zone would
//     drop edges the loaded topology still needs.
//
// An empty result is a legitimate outcome — a requested zone no backend
// declares — and the caller renders it as an empty vector, never an error.
func (t *Table) Select(f Family, az []string) []Backend {
	if t == nil {
		return nil
	}
	zoneRouted := f.AcceptsAZ() && hasValue(az)
	out := make([]Backend, 0, len(t.backends))
	for _, b := range t.backends {
		if !b.serves(f) {
			continue
		}
		if zoneRouted && !b.coversZone(az) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// String renders the table for logs — one entry per backend, never a
// credential value.
func (t *Table) String() string {
	if t == nil {
		return "[]"
	}
	parts := make([]string, len(t.backends))
	for i, b := range t.backends {
		parts[i] = b.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// DefaultBackendName is the name the implicit single-backend table uses. It
// appears in logs, in the per-backend failure metric, and on query spans.
const DefaultBackendName = "default"

// SingleBackendTable synthesises the implicit routing table a deployment with
// no routing file runs on: one backend named "default", addressed at the given
// endpoint, serving EVERY family, with no zones (a catch-all).
//
// It is a table rather than a separate unrouted code path so the compatibility
// claim is exercised by the whole existing test suite: every current unit,
// component, golden, and integration test runs through the router in this
// degenerate configuration.
//
// It lives beside the Table it constructs rather than beside the file parser
// because it needs no parser and no file I/O — it is what an embedder with a
// single upstream endpoint wants (design D14).
func SingleBackendTable(promURL, username, password string) (*Table, error) {
	return NewTable([]Backend{
		NewBackend(DefaultBackendName, promURL, Families, nil, username, password),
	})
}
