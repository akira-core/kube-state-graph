package kubegraph

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// ParseError is a typed request-parsing failure. Reason is a stable,
// machine-readable code (e.g. "missing_start", "invalid_range") that a caller
// maps to an HTTP status + response body; Message is the human-readable detail.
// Every ParseError corresponds to an HTTP 400 in kube-state-graph's API.
type ParseError struct {
	Reason  string
	Message string
}

func (e *ParseError) Error() string { return e.Message }

// ParseValues parses the /v1/graph query parameters into a build window
// (start, end) and a projection Scope. It is the single source of truth for the
// request contract, shared by the kube-state-graph HTTP handler and by any
// embedding application (via Engine.BuildFromValues), so the two can never
// drift. It performs no I/O and is independent of any HTTP framework.
//
// On failure it returns a *ParseError carrying the stable reason code.
func ParseValues(v url.Values) (start, end time.Time, scope graph.Scope, err error) {
	startStr := v.Get("start")
	endStr := v.Get("end")
	if startStr == "" {
		return start, end, scope, &ParseError{"missing_start", "start query parameter is required"}
	}
	if endStr == "" {
		return start, end, scope, &ParseError{"missing_end", "end query parameter is required"}
	}
	start, perr := parseTimestamp(startStr)
	if perr != nil {
		return start, end, scope, &ParseError{"invalid_start", perr.Error()}
	}
	end, perr = parseTimestamp(endStr)
	if perr != nil {
		return start, end, scope, &ParseError{"invalid_end", perr.Error()}
	}
	if !end.After(start) {
		return start, end, scope, &ParseError{"invalid_range", "end must be after start"}
	}

	depth := 0
	if s := v.Get("depth"); s != "" {
		d, derr := strconv.Atoi(s)
		if derr != nil {
			return start, end, scope, &ParseError{"invalid_depth", "depth must be an integer"}
		}
		depth = d
	}
	if depth < 0 {
		// strconv.Atoi accepts a negative integer; reject it here with the same
		// reason code as a non-integer depth (graph.NewScope would otherwise
		// surface it as the less-specific invalid_scope).
		return start, end, scope, &ParseError{"invalid_depth", "depth must be non-negative"}
	}
	if depth > graph.MaxTraversalDepth {
		return start, end, scope, &ParseError{"depth_too_large", "depth exceeds maximum"}
	}

	// Unknown ?edge_type= values are rejected by graph.NewScope itself
	// (validated against the registry /v1/edge-types serves), so D32 embedders
	// constructing scopes directly get the same 400-not-silent-empty guard;
	// the error surfaces below as the usual invalid_scope ParseError.
	scope, serr := graph.NewScope(
		v["cluster"],
		v["namespace"],
		v["edge_type"],
		v["name"],
		v.Get("root"),
		depth,
		v.Get("direction"),
	)
	if serr != nil {
		return start, end, scope, &ParseError{"invalid_scope", serr.Error()}
	}
	return start, end, scope, nil
}

// parseTimestamp accepts an RFC 3339 timestamp or Unix seconds, returning UTC.
func parseTimestamp(s string) (time.Time, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("timestamp must be RFC 3339 or Unix seconds: %q", s)
}
