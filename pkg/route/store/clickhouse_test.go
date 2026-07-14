package store

import (
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	vfBase   = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	sentinel = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

func gwRow(name string, vf, vt time.Time, seq uint64, hosts ...string) GatewayRow {
	return GatewayRow{
		Cluster: "c1", Namespace: "istio-system", Name: name,
		ValidFrom: vf, ValidTo: vt, IngestSeq: seq, ServerHosts: hosts,
	}
}

func gwVer(r GatewayRow) versionRow {
	return versionRow{cluster: r.Cluster, ns: r.Namespace, name: r.Name, vf: r.ValidFrom, vt: r.ValidTo, seq: r.IngestSeq}
}

// The core rewrite-close case: a stale open row (sentinel valid_to) and its
// closing rewrite share one version slot; the higher ingest_seq must win
// regardless of arrival order, and the collapse must be counted.
func TestDedupLatest_RewritePairCollapsesToClosingRow(t *testing.T) {
	stale := gwRow("gw", vfBase, sentinel, 3, "stale.example.com")
	closing := gwRow("gw", vfBase, vfBase.Add(time.Hour), 4, "stale.example.com")

	for name, rows := range map[string][]GatewayRow{
		"stale_first":   {stale, closing},
		"closing_first": {closing, stale},
	} {
		t.Run(name, func(t *testing.T) {
			s := &CH{}
			out := dedupOverlapCounted(s, rows, gwVer, vfBase.Add(2*time.Hour), vfBase.Add(3*time.Hour))
			assert.Empty(t, out, "the closed version must not overlap a post-close window")
			assert.Equal(t, uint64(1), s.CollapsedRows(), "the slot collapse must be counted")
		})
	}
}

// Distinct versions (distinct valid_from) are distinct slots — never collapsed.
func TestDedupLatest_DistinctVersionsSurvive(t *testing.T) {
	v1 := gwRow("gw", vfBase, vfBase.Add(time.Hour), 3, "a.example.com")
	v2 := gwRow("gw", vfBase.Add(time.Hour), sentinel, 4, "b.example.com")

	s := &CH{}
	out := dedupOverlapCounted(s, []GatewayRow{v1, v2}, gwVer, vfBase, sentinel)
	assert.Len(t, out, 2)
	assert.Zero(t, s.CollapsedRows(), "distinct slots are not collapses")
}

func TestVersionRowOverlapsWindow(t *testing.T) {
	r := versionRow{vf: vfBase, vt: vfBase.Add(time.Hour)}
	assert.True(t, r.overlapsWindow(vfBase.Add(-time.Minute), vfBase.Add(time.Minute)))
	assert.False(t, r.overlapsWindow(vfBase.Add(time.Hour), vfBase.Add(2*time.Hour)),
		"window starting exactly at valid_to must not overlap (half-open interval)")
	assert.False(t, r.overlapsWindow(vfBase.Add(-time.Hour), vfBase),
		"window ending exactly at valid_from must not overlap")
}

// dt64Lit must preserve milliseconds and survive the far-future sentinel —
// the two things the `?` time.Time bind breaks (second-precision toDateTime,
// which truncates ms and saturates past DateTime's 2106 ceiling).
func TestDT64Lit(t *testing.T) {
	assert.Equal(t,
		"toDateTime64('2026-05-01 10:00:00.500', 3, 'UTC')",
		dt64Lit(vfBase.Add(500*time.Millisecond)))
	assert.Equal(t,
		"toDateTime64('2200-01-01 00:00:00.000', 3, 'UTC')",
		dt64Lit(sentinel))
	// Non-UTC input renders in UTC.
	loc := time.FixedZone("UTC+8", 8*3600)
	assert.Equal(t,
		"toDateTime64('2026-05-01 10:00:00.000', 3, 'UTC')",
		dt64Lit(vfBase.In(loc)))
}

func TestPrune(t *testing.T) {
	def := &CH{}
	assert.Empty(t, def.prune(vfBase), "default (rewrite-compatible) mode must NOT filter valid_to in SQL")

	pruned := &CH{uniqueRows: true}
	assert.Equal(t,
		" AND toDateTime64('2026-05-01 10:00:00.000', 3, 'UTC') < valid_to",
		pruned.prune(vfBase))
}

// WithAuth must override DSN-embedded (or default) credentials after ParseDSN
// so secrets can live in env-only config rather than the DSN URL.
func TestApplyAuth_OverridesDSNUserinfo(t *testing.T) {
	chOpts, err := clickhouse.ParseDSN("clickhouse://embedded:oldpass@localhost:9000/routing")
	require.NoError(t, err)
	assert.Equal(t, "embedded", chOpts.Auth.Username)
	assert.Equal(t, "oldpass", chOpts.Auth.Password)

	applyAuth(chOpts, openConfig{username: "ksg", password: "env-secret"})
	assert.Equal(t, "ksg", chOpts.Auth.Username)
	assert.Equal(t, "env-secret", chOpts.Auth.Password)
	assert.Equal(t, "routing", chOpts.Auth.Database, "database from DSN must be preserved")
}

func TestApplyAuth_EmptyUsernameLeavesDSNUntouched(t *testing.T) {
	chOpts, err := clickhouse.ParseDSN("clickhouse://embedded:oldpass@localhost:9000/routing")
	require.NoError(t, err)

	applyAuth(chOpts, openConfig{})
	assert.Equal(t, "embedded", chOpts.Auth.Username)
	assert.Equal(t, "oldpass", chOpts.Auth.Password)
}

func TestWithAuth_OptionSetsOpenConfig(t *testing.T) {
	var cfg openConfig
	WithAuth("ksg", "s3cret")(&cfg)
	assert.Equal(t, "ksg", cfg.username)
	assert.Equal(t, "s3cret", cfg.password)
}

func TestWithUniqueRows_OptionSetsOpenConfig(t *testing.T) {
	var cfg openConfig
	WithUniqueRows()(&cfg)
	assert.True(t, cfg.uniqueRows)
}
