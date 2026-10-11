package kubegraph

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
// `prune` is projection-only.
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
// `depth`, `direction` and `edge_type` parameters degrade: an old client
// receives the unanchored, unfiltered view rather than an error. A withdrawn
// parameter's VALUE is never inspected, so a value that used to be rejected
// (an unregistered `edge_type`) is now simply ignored.
//
// On failure it returns a *ParseError carrying the stable reason code.
func ParseValues(v url.Values) (Request, error) {
	var req Request

	start, end, err := parseWindow(v)
	if err != nil {
		return req, err
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

	// Inventory is the INVERSE of `prune` so the zero Scope keeps prune on.
	req.Scope = graph.NewScope(v["cluster"], v["namespace"], !prune)
	req.Selector = promql.Selector{
		AZ:        v["az"],
		Env:       v["env"],
		Cluster:   v["cluster"],
		Namespace: v["namespace"],
	}
	return req, nil
}

// StorageRequest is the parsed /v1/storage-graph request: a build window, the
// upstream selector (az and env required, each one or more values), and the
// storage projection scope.
type StorageRequest struct {
	Start    time.Time
	End      time.Time
	Scope    graph.StorageScope
	Selector promql.Selector
}

// ParseStorageValues parses the /v1/storage-graph query parameters. It shares
// timestamp and selector-value validation with ParseValues so the two
// endpoints cannot drift on those contracts. az and env are required and
// repeatable — the selected zones are their Cartesian product; `prune` and
// every unknown parameter — including the withdrawn `edge_type` — are ignored.
func ParseStorageValues(v url.Values) (StorageRequest, error) {
	var req StorageRequest

	start, end, err := parseWindow(v)
	if err != nil {
		return req, err
	}
	req.Start, req.End = start, end

	az, err := atLeastOne("az", v["az"])
	if err != nil {
		return req, err
	}
	env, err := atLeastOne("env", v["env"])
	if err != nil {
		return req, err
	}

	for _, p := range []string{"cluster", "namespace", "az", "env", "ontap_cluster", "ontap_node", "node", "aggr", "svm", "pod", "pvc", "pv", "application"} {
		if err := validateSelectorValues(p, v[p]); err != nil {
			return req, err
		}
	}

	kind, values, rerr := oneStorageRoot(v)
	if rerr != nil {
		return req, rerr
	}
	// oneStorageRoot returned a kind holding at least one non-empty value, and
	// NewStorageScope drops only empty values (a malformed pod or pvc is an error), so
	// the scope always carries a root here.
	scope, serr := graph.NewStorageScope(v["cluster"], v["namespace"], kind, values)
	if serr != nil {
		return req, &ParseError{"invalid_scope", serr.Error()}
	}
	req.Scope = scope
	req.Selector = promql.Selector{
		AZ:        az,
		Env:       env,
		Cluster:   v["cluster"],
		Namespace: v["namespace"],
	}
	return req, nil
}

// storageRootParams is the root parameters in the order a mixed-kind error
// names them.
var storageRootParams = []string{
	"ontap_cluster", "ontap_node", "aggr", "svm", "node", "pod", "pvc", "pv", "application",
}

// oneStorageRoot returns the single root kind that carries a non-empty value.
// Zero kinds is missing_root; two or more is invalid_scope naming those
// parameters. Empty values do not count, so a bare `?aggr=` is not a root.
func oneStorageRoot(v url.Values) (graph.StorageRootKind, []string, error) {
	var present []string
	for _, p := range storageRootParams {
		if hasNonEmpty(v[p]) {
			present = append(present, p)
		}
	}
	switch len(present) {
	case 0:
		return "", nil, &ParseError{"missing_root", "a storage-graph request requires exactly one root kind"}
	case 1:
		p := present[0]
		return graph.StorageRootKind(p), v[p], nil
	default:
		return "", nil, &ParseError{"invalid_scope", "exactly one root kind is allowed, got " + strings.Join(present, " and ")}
	}
}

func hasNonEmpty(values []string) bool {
	for _, v := range values {
		if v != "" {
			return true
		}
	}
	return false
}

// parseWindow reads the required start / end pair shared by every graph
// endpoint: RFC 3339 or Unix seconds, and only `end > start` is enforced.
// The reason codes are the wire contract both parsers must keep.
func parseWindow(v url.Values) (start, end time.Time, err error) {
	startStr := v.Get("start")
	endStr := v.Get("end")
	if startStr == "" {
		return start, end, &ParseError{"missing_start", "start query parameter is required"}
	}
	if endStr == "" {
		return start, end, &ParseError{"missing_end", "end query parameter is required"}
	}
	start, perr := parseTimestamp(startStr)
	if perr != nil {
		return start, end, &ParseError{"invalid_start", perr.Error()}
	}
	end, perr = parseTimestamp(endStr)
	if perr != nil {
		return start, end, &ParseError{"invalid_end", perr.Error()}
	}
	if !end.After(start) {
		return start, end, &ParseError{"invalid_range", "end must be after start"}
	}
	return start, end, nil
}

// atLeastOne requires a selector parameter to carry at least one non-empty
// value and returns the full value set, sorted and de-duplicated so the order a
// client sends them in never changes the parsed request. Absence is
// missing_<param> (the message names the parameter so a client can tell az from
// env).
func atLeastOne(param string, values []string) ([]string, error) {
	got := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			got = append(got, v)
		}
	}
	if len(got) == 0 {
		return nil, &ParseError{"missing_" + param, param + " query parameter is required"}
	}
	slices.Sort(got)
	return slices.Compact(got), nil
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
