// Package matchcheck drives Envoy's router_check_tool (offline, no traffic, no
// Envoy process) over istiod-translated RouteConfigurations to resolve each
// host+path to its destination cluster. It is the sole route-matching engine:
// reimplementing Envoy's RDS semantics in Go was rejected because the whole
// correctness argument rests on the matcher being Envoy's own (design D8).
//
// Ported from poc/route2a/internal/matchcheck with the docker fallback DROPPED:
// the tool runs as a native binary only, its path injected from configuration
// (--router-check-bin; the container image copies it out of the Envoy tools
// image at build time). router_check_tool takes one RouteConfiguration + a
// batch of test cases, so callers invoke it once per gateway.
package matchcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// routerCheckTest is one entry of the router_check_tool test file.
type routerCheckTest struct {
	TestName string  `json:"test_name"`
	Input    rcInput `json:"input"`
	Validate rcValid `json:"validate"`
}

type rcInput struct {
	Authority string `json:"authority"`
	Path      string `json:"path"`
	Method    string `json:"method"`
}

type rcValid struct {
	ClusterName string `json:"cluster_name"`
}

// Runner executes a native router_check_tool binary over a RouteConfiguration
// plus a batch of test cases.
type Runner struct {
	bin string
}

// NewRunner binds a Runner to the router_check_tool binary at bin and verifies
// it actually executes on this host (fail fast at startup, not on the first
// build that needs it). An empty bin resolves via PATH.
func NewRunner(bin string) (Runner, error) {
	if bin == "" {
		p, err := exec.LookPath("router_check_tool")
		if err != nil {
			return Runner{}, fmt.Errorf("router_check_tool not found on PATH and no path configured: %w", err)
		}
		bin = p
	}
	// --version exits 0 and proves the binary runs on this OS/arch (a Linux
	// ELF on the wrong host fails here, not mid-build).
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		return Runner{}, fmt.Errorf("router_check_tool %q not executable: %w: %s", bin, err, bytes.TrimSpace(out))
	}
	return Runner{bin: bin}, nil
}

// Query is one host+path to resolve (method is fixed to GET).
type Query struct {
	Host string
	Path string
}

// resolveSentinel is an expected cluster value no real route can equal, so
// router_check_tool reports every case as a (forced) mismatch and prints the
// real matched cluster in its "actual: [...]" detail — turning the validator
// into a resolver.
const resolveSentinel = "__routecheck_unmatched_sentinel__"

// Resolve returns, per query, the destination cluster that rc routes it to
// ("" = no route / miss), using router_check_tool as the matching engine. It
// needs no expected answer. One tool invocation covers the whole batch.
func (r Runner) Resolve(ctx context.Context, rc *route.RouteConfiguration, queries []Query) ([]string, error) {
	out := make([]string, len(queries))
	if len(queries) == 0 {
		return out, nil
	}

	work, err := os.MkdirTemp("", "rc-resolve-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	rcJSON, err := protojson.Marshal(rc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(work, "rc.json"), rcJSON, 0o600); err != nil {
		return nil, err
	}

	tests := make([]routerCheckTest, len(queries))
	for i, q := range queries {
		tests[i] = routerCheckTest{
			TestName: strconv.Itoa(i),
			Input:    rcInput{Authority: q.Host, Path: q.Path, Method: "GET"},
			Validate: rcValid{ClusterName: resolveSentinel},
		}
	}
	testsJSON, _ := json.Marshal(map[string]any{"tests": tests})
	if err := os.WriteFile(filepath.Join(work, "tests.json"), testsJSON, 0o600); err != nil {
		return nil, err
	}

	// The tool exits non-zero because every case "fails" the sentinel — expected.
	// A genuine failure (config won't load) yields no per-case detail lines,
	// which parseActuals reports as an error.
	// --disable-deprecation-check is always set: istiod-translated RCs carry
	// deprecated fields (e.g. RouteAction.max_grpc_timeout) that newer Envoy
	// otherwise rejects at load.
	// #nosec G204 -- r.bin is the operator-configured --router-check-bin path
	// (verified executable at startup); the remaining args are fixed flags plus
	// temp-file paths this process just created.
	raw, runErr := exec.CommandContext(ctx, r.bin,
		"-c", filepath.Join(work, "rc.json"),
		"-t", filepath.Join(work, "tests.json"),
		"--details", "--disable-deprecation-check",
	).CombinedOutput()
	clusters, perr := parseActuals(raw, len(queries))
	if perr != nil {
		return nil, fmt.Errorf("router_check_tool resolve: %w: %w\n%s", perr, runErr, raw)
	}
	copy(out, clusters)
	return out, nil
}

var actualMarker = []byte("actual: [")

// parseActuals reads router_check_tool --details output. Each case prints its
// test_name (our decimal index) on its own line, followed by a line containing
// "actual: [<cluster>]" (empty brackets == miss). Returns one cluster per index.
//
// It errors unless EVERY query was recovered. The marker rule — any all-digits
// line re-points the current index — cannot distinguish a test name from a stray
// numeric line the tool prints for its own reasons, and the scraped text format
// is not a stable contract (design D8 rests on the MATCHER being Envoy's, not on
// its output format). Today only a single query is ever posed, so a stray line
// already fails closed; requiring a full result set makes that hold for any
// batch size instead of by accident (design D7).
func parseActuals(out []byte, n int) ([]string, error) {
	res := make([]string, n)
	filled := make([]bool, n)
	got := 0
	cur := -1
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := bytes.TrimSpace(line)
		if idx, err := strconv.Atoi(string(s)); err == nil {
			if idx >= 0 && idx < n {
				cur = idx
			} else {
				cur = -1
			}
			continue
		}
		if i := bytes.Index(s, actualMarker); i >= 0 && cur >= 0 {
			rest := s[i+len(actualMarker):]
			if j := bytes.IndexByte(rest, ']'); j >= 0 {
				if !filled[cur] {
					res[cur] = string(rest[:j])
					filled[cur] = true
					got++
				}
			}
			cur = -1
		}
	}
	if got != n {
		return nil, fmt.Errorf("parsed %d of %d per-case results", got, n)
	}
	return res, nil
}
