package build

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func appMetaResolve(vec model.Vector) appMetadataIndex {
	return resolveAppMetadata(vec, defaultAppMetadata(), newClusterResolver(promql.LabelKeys{}))
}

func shopRow(pod, app string, at model.SampleValue, extra map[string]string) *model.Sample {
	labels := appMetaBase(pod, app, "", "")
	for k, v := range extra {
		labels[k] = v
	}
	return appMetaSample(labels, at)
}

func forEachOrder(t *testing.T, rows []*model.Sample, fn func(*testing.T, model.Vector)) {
	t.Helper()
	n := len(rows)
	var walk func(int)
	walk = func(k int) {
		if k == 1 {
			fn(t, slices.Clone(rows))
			return
		}
		walk(k - 1)
		for i := range k - 1 {
			if k%2 == 0 {
				rows[i], rows[k-1] = rows[k-1], rows[i]
			} else {
				rows[0], rows[k-1] = rows[k-1], rows[0]
			}
			walk(k - 1)
		}
	}
	walk(n)
}

func TestResolveAppMetadata_NamespaceGroup(t *testing.T) {
	t.Parallel()
	key := appNamespaceKey{"us-dev-c1", "shop"}

	t.Run("latest group wins with its own owner", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"namespace_group": "retail", "namespace_group_owner": "alice"}),
			shopRow("web-2", "checkout", 2, map[string]string{"namespace_group": "commerce", "namespace_group_owner": "bob"}),
		})
		assert.Equal(t, nsGroupPick{name: "commerce", owner: "bob"}, got.nsGroups[key])
	})

	t.Run("owner is never borrowed", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"namespace_group": "retail", "namespace_group_owner": "alice"}),
			shopRow("web-2", "checkout", 2, map[string]string{"namespace_group": "commerce"}),
		})
		assert.Equal(t, nsGroupPick{name: "commerce"}, got.nsGroups[key])
	})

	t.Run("owner without a group is ignored", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"namespace_group_owner": "alice"}),
		})
		assert.NotContains(t, got.nsGroups, key)
	})

	t.Run("tied rows pick the owner deterministically", func(t *testing.T) {
		t.Parallel()
		rows := []*model.Sample{
			shopRow("web-1", "checkout", 5, map[string]string{"namespace_group": "commerce", "namespace_group_owner": ""}),
			shopRow("web-2", "checkout", 5, map[string]string{"namespace_group": "commerce", "namespace_group_owner": "carol"}),
			shopRow("web-3", "checkout", 5, map[string]string{"namespace_group": "commerce", "namespace_group_owner": "bob"}),
		}
		forEachOrder(t, rows, func(t *testing.T, vec model.Vector) {
			t.Helper()
			got := appMetaResolve(vec)
			assert.Equal(t, nsGroupPick{name: "commerce", owner: "bob"}, got.nsGroups[key])
		})
	})

	t.Run("empty pod still contributes", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("", "checkout", 1, map[string]string{"namespace_group": "commerce"}),
		})
		assert.Equal(t, nsGroupPick{name: "commerce"}, got.nsGroups[key])
		assert.Empty(t, got.podApps)
	})
}

func TestResolveAppMetadata_NamespaceAlias(t *testing.T) {
	t.Parallel()
	key := appNamespaceKey{"us-dev-c1", "shop"}

	t.Run("latest alias wins", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"namespace_alias": "Shop (old)"}),
			shopRow("web-2", "checkout", 2, map[string]string{"namespace_alias": "Shop"}),
		})
		assert.Equal(t, "Shop", got.nsAliases[key])
	})

	t.Run("alias does not follow the group's row", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"namespace_alias": "Shop"}),
			shopRow("web-2", "checkout", 2, map[string]string{"namespace_group": "commerce"}),
		})
		assert.Equal(t, nsGroupPick{name: "commerce"}, got.nsGroups[key])
		assert.Equal(t, "Shop", got.nsAliases[key])
	})
}

func TestResolveAppMetadata_DeployTime(t *testing.T) {
	t.Parallel()
	key := appDeployerKey{"us-dev-c1", "shop", "checkout"}
	want := time.Date(2025, 10, 8, 13, 14, 15, 0, time.UTC)

	t.Run("paired with the deployer", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"user": "alice", "deploy_time": "2025/10/01 08:00:00"}),
			shopRow("web-2", "checkout", 2, map[string]string{"user": "bob", "deploy_time": "2025/10/08 13:14:15"}),
		})
		assert.Equal(t, "bob", got.deployers[key])
		assert.True(t, got.deployTimes[key].Equal(want))
	})

	t.Run("never borrowed from another row", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"user": "alice", "deploy_time": "2025/10/01 08:00:00"}),
			shopRow("web-2", "checkout", 2, map[string]string{"user": "bob"}),
		})
		assert.Equal(t, "bob", got.deployers[key])
		assert.Zero(t, got.deployTimes[key])
	})

	t.Run("empty user contributes no deploy time", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{"deploy_time": "2025/10/08 13:14:15"}),
		})
		assert.NotContains(t, got.deployers, key)
		assert.Zero(t, got.deployTimes[key])
	})

	t.Run("tied rows pick the later instant", func(t *testing.T) {
		t.Parallel()
		rows := []*model.Sample{
			shopRow("web-1", "checkout", 5, map[string]string{"user": "bob", "deploy_time": "2025/10/01 08:00:00"}),
			shopRow("web-2", "checkout", 5, map[string]string{"user": "bob", "deploy_time": "2025/10/08 13:14:15"}),
		}
		forEachOrder(t, rows, func(t *testing.T, vec model.Vector) {
			t.Helper()
			got := appMetaResolve(vec)
			assert.Equal(t, "bob", got.deployers[key])
			assert.True(t, got.deployTimes[key].Equal(want))
		})
	})

	t.Run("unparsable tied row does not mask a parsable one", func(t *testing.T) {
		t.Parallel()
		rows := []*model.Sample{
			shopRow("web-1", "checkout", 5, map[string]string{"user": "bob", "deploy_time": "yesterday"}),
			shopRow("web-2", "checkout", 5, map[string]string{"user": "bob", "deploy_time": "2025/10/08 13:14:15"}),
		}
		forEachOrder(t, rows, func(t *testing.T, vec model.Vector) {
			t.Helper()
			got := appMetaResolve(vec)
			assert.Equal(t, "bob", got.deployers[key])
			assert.True(t, got.deployTimes[key].Equal(want))
			assert.Equal(t, 1, got.unparsedDeployTimes)
		})
	})

	t.Run("values outside the two forms stay unresolved", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{
			"2025/10/08 9:14:15",
			"2025/10/08 13:14:15.5",
			" 2025/10/08 13:14:15",
			"1759929255",
			"2025/02/30 00:00:00",
		} {
			got := appMetaResolve(model.Vector{
				shopRow("web-1", "checkout", 1, map[string]string{"user": "bob", "deploy_time": raw}),
			})
			assert.Equal(t, "bob", got.deployers[key], raw)
			assert.Zero(t, got.deployTimes[key], raw)
		}
	})

	t.Run("RFC 3339 converts to UTC", func(t *testing.T) {
		t.Parallel()
		got := appMetaResolve(model.Vector{
			shopRow("web-1", "checkout", 1, map[string]string{
				"user": "bob", "deploy_time": "2025-10-08T13:14:15.987+08:00",
			}),
		})
		assert.Equal(t, "bob", got.deployers[key])
		assert.True(t, got.deployTimes[key].Equal(time.Date(2025, 10, 8, 5, 14, 15, 0, time.UTC)))
		assert.Zero(t, got.unparsedDeployTimes)
	})
}

func TestParseTopology_UnparsedDeployTimeDebug(t *testing.T) {
	src := defaultAppMetadata()
	parse := func(vec model.Vector) {
		parseTopology(topologyVectors{AppMetadata: vec, AppMetadataSource: &src}, promql.LabelKeys{})
	}
	cohort := func(raws ...string) model.Vector {
		vec := make(model.Vector, 0, len(raws))
		for i, raw := range raws {
			extra := map[string]string{"user": "bob"}
			if raw != "" {
				extra["deploy_time"] = raw
			}
			vec = append(vec, shopRow("web-"+string(rune('1'+i)), "checkout", 5, extra))
		}
		return vec
	}

	t.Run("one record counts the winning cohort", func(t *testing.T) {
		recs := captureDebugRecords(t, func() {
			// An older alice row and a different user's garbage do not count.
			vec := cohort("yesterday", "2025/10/08 13:14:15")
			vec = append(vec,
				shopRow("web-9", "checkout", 1, map[string]string{"user": "alice", "deploy_time": "not-a-time"}),
				shopRow("web-8", "checkout", 5, map[string]string{"user": "carol", "deploy_time": "nope"}),
			)
			parse(vec)
		})
		var matched []map[string]any
		for _, r := range recs {
			if r["msg"] == "app_metadata_deploy_time_unparsed" {
				matched = append(matched, r)
			}
		}
		require.Len(t, matched, 1)
		assert.InDelta(t, 1.0, matched[0]["count"], 1e-9)
	})

	t.Run("every value parsing logs nothing", func(t *testing.T) {
		recs := captureDebugRecords(t, func() {
			parse(cohort("2025/10/08 13:14:15"))
		})
		for _, r := range recs {
			assert.NotEqual(t, "app_metadata_deploy_time_unparsed", r["msg"])
		}
	})

	t.Run("absent label logs nothing", func(t *testing.T) {
		recs := captureDebugRecords(t, func() {
			parse(model.Vector{shopRow("web-1", "checkout", 5, map[string]string{"user": "bob"})})
		})
		for _, r := range recs {
			assert.NotEqual(t, "app_metadata_deploy_time_unparsed", r["msg"])
		}
	})
}
