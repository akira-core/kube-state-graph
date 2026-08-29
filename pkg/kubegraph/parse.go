package kubegraph

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
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

// Request is the parsed /v1/graph request: a build window, the upstream
// selector the build pushes into PromQL, and the projection scope applied to
// the built graph.
//
// `cluster` and `namespace` deliberately appear in BOTH Selector and Scope —
// they narrow the queries at the source and are re-applied over the result as
// defence in depth. `az` / `env` are selector-only (no node carries them), and
// `edge_type` / `prune` are projection-only.
type Request struct {
	Start    time.Time
	End      time.Time
	Scope    graph.Scope
	Selector promql.Selector
}

// maxSelectorValueLen bounds a single selector value. 253 is the longest legal
// DNS subdomain (so every Kubernetes cluster / namespace name fits) and is a
// generous ceiling for the operator-defined zone / environment vocabularies.
const maxSelectorValueLen = 253

// ParseValues parses the /v1/graph query parameters into a Request. It is the
// single source of truth for the request contract, shared by the
// kube-state-graph HTTP handler and by any embedding application (via
// Engine.BuildFromValues), so the two can never drift. It performs no I/O and
// is independent of any HTTP framework.
//
// Unknown parameters are ignored, which is how the withdrawn `name`, `root`,
// `depth` and `direction` parameters degrade: an old client receives the
// unanchored view rather than an error.
//
// On failure it returns a *ParseError carrying the stable reason code.
func ParseValues(v url.Values) (Request, error) {
	var req Request

	startStr := v.Get("start")
	endStr := v.Get("end")
	if startStr == "" {
		return req, &ParseError{"missing_start", "start query parameter is required"}
	}
	if endStr == "" {
		return req, &ParseError{"missing_end", "end query parameter is required"}
	}
	start, perr := parseTimestamp(startStr)
	if perr != nil {
		return req, &ParseError{"invalid_start", perr.Error()}
	}
	end, perr := parseTimestamp(endStr)
	if perr != nil {
		return req, &ParseError{"invalid_end", perr.Error()}
	}
	if !end.After(start) {
		return req, &ParseError{"invalid_range", "end must be after start"}
	}
	req.Start, req.End = start, end

	// Selector-level dimensions are validated ONCE, here: promql.Render only
	// escapes, and an embedder constructing a Selector directly is trusted
	// code. Rejecting control characters and absurd lengths (quoting already
	// makes injection impossible) keeps a malformed request out of the
	// upstream query rather than turning it into an obscure store error.
	for _, p := range []string{"cluster", "namespace", "az", "env"} {
		if err := validateSelectorValues(p, v[p]); err != nil {
			return req, err
		}
	}

	prune, err := parsePrune(v.Get("prune"))
	if err != nil {
		return req, err
	}

	// Unknown ?edge_type= values are rejected by graph.NewScope itself
	// (validated against the registry /v1/edge-types serves), so D32 embedders
	// constructing scopes directly get the same 400-not-silent-empty guard;
	// the error surfaces below as the usual invalid_scope ParseError.
	//
	// Inventory is the INVERSE of `prune` so the zero Scope keeps prune on.
	scope, serr := graph.NewScope(v["cluster"], v["namespace"], v["edge_type"], !prune)
	if serr != nil {
		return req, &ParseError{"invalid_scope", serr.Error()}
	}
	req.Scope = scope
	req.Selector = promql.Selector{
		AZ:        v["az"],
		Env:       v["env"],
		Cluster:   v["cluster"],
		Namespace: v["namespace"],
	}
	return req, nil
}

// parsePrune reads the single-valued `prune` parameter. Absent ⇒ true (the
// default connectivity prune); anything other than the two literals is a 400
// rather than a silently-ignored typo that would return the wrong graph.
func parsePrune(raw string) (bool, error) {
	switch raw {
	case "":
		return true, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &ParseError{"invalid_scope", fmt.Sprintf("prune must be true or false, got %q", raw)}
	}
}

// validateSelectorValues rejects values that must never reach an upstream
// query. Empty values are skipped rather than rejected: a bare `?namespace=`
// is a no-op, matching graph.Scope's own set construction.
func validateSelectorValues(param string, values []string) error {
	for _, val := range values {
		if val == "" {
			continue
		}
		if len(val) > maxSelectorValueLen {
			return &ParseError{"invalid_scope", fmt.Sprintf("%s value exceeds %d bytes", param, maxSelectorValueLen)}
		}
		// Checked BEFORE the control-character scan, which cannot see this:
		// ranging over a string decodes an invalid byte as U+FFFD, and
		// RuneError is not a control rune. The raw byte would then survive
		// escapeLiteral (it is neither a quote nor a backslash) and reach
		// VictoriaMetrics inside a PromQL string literal, where the parse
		// error surfaces as a 502 upstream failure instead of the 400 this
		// validator exists to produce.
		if !utf8.ValidString(val) {
			return &ParseError{"invalid_scope", fmt.Sprintf("%s value is not valid UTF-8", param)}
		}
		for _, r := range val {
			if unicode.IsControl(r) {
				return &ParseError{"invalid_scope", fmt.Sprintf("%s value contains a control character", param)}
			}
		}
	}
	return nil
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
