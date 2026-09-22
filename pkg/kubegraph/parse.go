package kubegraph

import (
	"fmt"
	"net/url"
	"slices"
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
// upstream selector (az and env always single-valued), and the storage
// projection scope.
type StorageRequest struct {
	Start    time.Time
	End      time.Time
	Scope    graph.StorageScope
	Selector promql.Selector
}

// ParseStorageValues parses the /v1/storage-graph query parameters. It shares
// timestamp and selector-value validation with ParseValues so the two
// endpoints cannot drift on those contracts. az and env are required and
// single-valued; `prune` and every unknown parameter — including the
// withdrawn `edge_type` — are ignored.
func ParseStorageValues(v url.Values) (StorageRequest, error) {
	var req StorageRequest

	start, end, err := parseWindow(v)
	if err != nil {
		return req, err
	}
	req.Start, req.End = start, end

	az, err := exactlyOne("az", v["az"])
	if err != nil {
		return req, err
	}
	env, err := exactlyOne("env", v["env"])
	if err != nil {
		return req, err
	}

	for _, p := range []string{"cluster", "namespace", "az", "env", "ontap_cluster", "node", "aggr", "svm", "pod", "application"} {
		if err := validateSelectorValues(p, v[p]); err != nil {
			return req, err
		}
	}

	scope, serr := graph.NewStorageScope(
		v["cluster"], v["namespace"],
		v["ontap_cluster"], v["node"], v["aggr"], v["svm"], v["pod"], v["application"],
	)
	if serr != nil {
		return req, &ParseError{"invalid_scope", serr.Error()}
	}
	req.Scope = scope
	req.Selector = promql.Selector{
		AZ:        []string{az},
		Env:       []string{env},
		Cluster:   v["cluster"],
		Namespace: deriveStorageNamespaces(scope, v["namespace"]),
	}
	return req, nil
}

// deriveStorageNamespaces returns the namespace selector a storage request's
// build is narrowed by: the explicit `namespace` parameter whenever it carries a
// value, and otherwise — when every root is a pod root — the roots' own
// namespaces.
//
// The derived case is output-preserving, which is what lets the parser push it
// upstream unasked. With pod roots only, a retained path is anchored on a root
// pod; its claim lives in that pod's namespace (a pod can only reference a claim
// in its own namespace), and every other pod on the path mounts that same claim.
// Nothing a pod-rooted body draws lies outside the roots' namespaces, so reading
// only those namespaces changes the queries and never the body.
//
// Any storage-side, `node` or `application` root suppresses it: those roots
// select paths in every namespace (an Application is not bound to one). An
// explicit namespace is never widened, intersected or replaced — an
// intersection could come out empty, which the selector reads as "no filter",
// the one outcome that would WIDEN the read. The projection's own namespace
// filter (StorageScope.Namespaces) is untouched: this narrows the upstream
// read only.
func deriveStorageNamespaces(scope graph.StorageScope, explicit []string) []string {
	if slices.ContainsFunc(explicit, func(ns string) bool { return ns != "" }) {
		return explicit
	}
	if scope.Roots.RequestedStorage() || len(scope.Roots.Applications) > 0 || len(scope.Roots.Pods) == 0 {
		return explicit
	}
	namespaces := make([]string, 0, len(scope.Roots.Pods))
	for ref := range scope.Roots.Pods {
		namespaces = append(namespaces, ref.Namespace)
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
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

// exactlyOne requires a selector parameter to be present with exactly one
// non-empty value. Absence is missing_<param>; a second value is invalid_scope
// (the message names the parameter so a client can tell az from env).
func exactlyOne(param string, values []string) (string, error) {
	var got []string
	for _, v := range values {
		if v != "" {
			got = append(got, v)
		}
	}
	if len(got) == 0 {
		return "", &ParseError{"missing_" + param, param + " query parameter is required"}
	}
	if len(got) > 1 {
		return "", &ParseError{"invalid_scope", param + " must be single-valued"}
	}
	return got[0], nil
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
