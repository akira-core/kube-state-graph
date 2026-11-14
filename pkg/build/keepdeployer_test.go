package build

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

// keepDeployer's outcome must not depend on whether it parses a row's deploy
// time before or after deciding the row loses: a losing row neither moves the
// pick nor counts an unparsable time, whatever its raw value.
func TestKeepDeployer_LosingRowsLeaveThePickUntouched(t *testing.T) {
	key := appDeployerKey{"c", "shop", "orders"}
	want := deployerCandidate{
		user: "alice", at: 200, ok: true,
		when: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	cases := []struct {
		name, user, raw string
		at              model.SampleValue
	}{
		{"older, parsable", "bob", "2026/12/31 00:00:00", 100},
		{"older, unparsable", "bob", "yesterday", 100},
		{"same time, larger user, unparsable", "zed", "not-a-time", 200},
		{"same time, larger user, parsable", "zed", "2027-01-01T00:00:00Z", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := map[appDeployerKey]deployerCandidate{}
			keepDeployer(dst, key, "alice", "2026/01/02 03:04:05", 200)
			keepDeployer(dst, key, tc.user, tc.raw, tc.at)
			assert.Equal(t, want, dst[key])
		})
	}
}

// The winning cohort still refines its time and counts its own failures.
func TestKeepDeployer_WinningCohortRefines(t *testing.T) {
	key := appDeployerKey{"c", "shop", "orders"}
	dst := map[appDeployerKey]deployerCandidate{}
	keepDeployer(dst, key, "alice", "garbage", 200)
	keepDeployer(dst, key, "alice", "2026/01/02 03:04:05", 200)
	keepDeployer(dst, key, "alice", "2026-01-03T00:00:00Z", 200)
	keepDeployer(dst, key, "alice", "also-garbage", 200)
	got := dst[key]
	assert.True(t, got.ok)
	assert.Equal(t, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), got.when)
	assert.Equal(t, 2, got.unparsed)

	keepDeployer(dst, key, "bob", "", 300)
	assert.Equal(t, deployerCandidate{user: "bob", at: 300}, dst[key], "a newer row replaces the cohort")
}

// BenchmarkKeepDeployer is one application's history: the metric returns a
// row per past deploy, and only the newest wins. Every older row used to pay
// for parsing its deploy time before it was discarded.
func BenchmarkKeepDeployer(b *testing.B) {
	const rows = 1000
	type row struct {
		user, raw string
		at        model.SampleValue
	}
	in := make([]row, rows)
	for i := range rows {
		// Newest first, so every later row loses.
		in[i] = row{
			user: fmt.Sprintf("user-%03d", i%50),
			raw:  fmt.Sprintf("2026/01/%02d 03:04:05", 1+i%28),
			at:   model.SampleValue(rows - i),
		}
	}
	key := appDeployerKey{"c", "shop", "orders"}
	b.ReportAllocs()
	for b.Loop() {
		dst := map[appDeployerKey]deployerCandidate{}
		for _, r := range in {
			keepDeployer(dst, key, r.user, r.raw, r.at)
		}
	}
}
