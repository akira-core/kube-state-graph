package build

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// infoLogger installs an Info-level default logger for the test, so the
// allocation counts below do not depend on the ambient one.
func infoLogger(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// runtimeString returns s as a heap string the compiler cannot treat as a
// constant, so boxing it into an interface costs what a label read from a
// series costs.
func runtimeString(s string) string { return strings.Clone(s) }

func gateTrace() sgTrace {
	return sgTrace{
		side: "server", label: runtimeString("legacy-workload"),
		clientLabel: runtimeString("checkout"), serverLabel: runtimeString("legacy-workload"),
		clientUID: runtimeString("abc"), traceCluster: runtimeString("prod-1"),
	}
}

// With Debug off, recording an external fallback is a counter bump and
// nothing else: no argument slice, no boxed attribute.
func TestDebugOff_NoteExternalAllocatesNothing(t *testing.T) {
	infoLogger(t)
	r := newSGResolver(familyTopology(), false)
	tr := gateTrace()
	r.noteExternal("missing_uid_nonurl_label", tr) // the counter key exists from here on

	allocs := testing.AllocsPerRun(200, func() { r.noteExternal("missing_uid_nonurl_label", tr) })

	assert.Zero(t, allocs)
	// One priming call, plus AllocsPerRun's warm-up call and its 200 runs.
	assert.Equal(t, 202, r.extReasons["missing_uid_nonurl_label"], "the counter is not gated")
}

// The missing-UID fallback with Debug off allocates exactly what building the
// external node id allocates — the Debug record's attributes cost nothing.
func TestDebugOff_MissingUIDFallbackAllocatesOnlyTheNodeID(t *testing.T) {
	infoLogger(t)
	r := newSGResolver(familyTopology(), false)
	tr := gateTrace()
	label, anchor := runtimeString("legacy-workload"), runtimeString("prod-1")
	_ = r.resolveEmptyUID(label, anchor, tr)

	idOnly := testing.AllocsPerRun(200, func() { _ = r.external(label) })
	fallback := testing.AllocsPerRun(200, func() { _ = r.resolveEmptyUID(label, anchor, tr) })

	assert.InDelta(t, idOnly+1, fallback, 0, "one []string result, nothing for the Debug record")
}

// A "://" label shadowed by a populated UID costs nothing over the same
// resolution of a plain label when Debug is off, while the shadow counter
// still counts it.
func TestDebugOff_ShadowedConnStringAllocatesNothingExtra(t *testing.T) {
	infoLogger(t)
	r := newSGResolver(familyTopology(), false)
	tr := gateTrace()
	conn, plain := runtimeString("nats://nats.messaging.svc:4222"), runtimeString("checkout")
	cluster, uid, ns := runtimeString("prod-1"), runtimeString("abc"), runtimeString("shop")

	plainAllocs := testing.AllocsPerRun(200, func() { _, _ = r.resolveClient(plain, cluster, uid, ns, tr) })
	before := r.shadowed
	connAllocs := testing.AllocsPerRun(200, func() { _, _ = r.resolveClient(conn, cluster, uid, ns, tr) })

	assert.InDelta(t, plainAllocs, connAllocs, 0)
	assert.Equal(t, before+201, r.shadowed, "the shadow counter is not gated")
}
