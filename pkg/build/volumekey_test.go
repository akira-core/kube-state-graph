package build

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustRewriter(t *testing.T, rules []VolumeKeyRule, mode VolumeMatchMode) *VolumeKeyRewriter {
	t.Helper()
	rw, err := NewVolumeKeyRewriter(rules, mode)
	require.NoError(t, err)
	return rw
}

func TestNewVolumeKeyRewriter_DefaultsAndOrder(t *testing.T) {
	t.Run("nil rules adopt the default dash-to-underscore rule", func(t *testing.T) {
		rw := mustRewriter(t, nil, "")
		assert.Equal(t, DefaultVolumeMatchMode, rw.mode)
		assert.Equal(t, "pvc_9f3a_11d0", rw.token("pvc-9f3a-11d0"),
			"every dash is replaced, not just the first")
	})

	t.Run("empty non-nil rules are an explicit identity rewrite", func(t *testing.T) {
		rw := mustRewriter(t, []VolumeKeyRule{}, VolumeMatchExact)
		assert.Equal(t, "pvc-9f3a", rw.token("pvc-9f3a"))
	})

	t.Run("rules apply in declaration order", func(t *testing.T) {
		rw := mustRewriter(t, []VolumeKeyRule{
			{Pattern: "-", Replacement: "_"},
			{Pattern: "^", Replacement: "vol_"},
		}, "")
		assert.Equal(t, "vol_pvc_9f3a", rw.token("pvc-9f3a"))
	})

	t.Run("reversing the order changes the token", func(t *testing.T) {
		rw := mustRewriter(t, []VolumeKeyRule{
			{Pattern: "^", Replacement: "vol-"},
			{Pattern: "-", Replacement: "_"},
		}, "")
		assert.Equal(t, "vol_pvc_9f3a", rw.token("pvc-9f3a"),
			"the prefix rule ran first, so its own dash is rewritten too")
	})

	t.Run("capture groups are usable in the replacement", func(t *testing.T) {
		rw := mustRewriter(t, []VolumeKeyRule{
			{Pattern: `^pvc-(.*)$`, Replacement: "trident_${1}"},
			{Pattern: "-", Replacement: "_"},
		}, "")
		assert.Equal(t, "trident_9f3a_11d0", rw.token("pvc-9f3a-11d0"))
	})

	t.Run("an empty PV name derives no token", func(t *testing.T) {
		assert.Empty(t, mustRewriter(t, nil, "").token(""))
	})
}

func TestNewVolumeKeyRewriter_Errors(t *testing.T) {
	t.Run("uncompilable pattern is an error naming it", func(t *testing.T) {
		_, err := NewVolumeKeyRewriter([]VolumeKeyRule{{Pattern: "([", Replacement: "x"}}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"(["`, "the offending pattern is named")
	})

	t.Run("the offending rule is identified by position", func(t *testing.T) {
		_, err := NewVolumeKeyRewriter([]VolumeKeyRule{
			{Pattern: "-", Replacement: "_"},
			{Pattern: "*bad", Replacement: "x"},
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rule 2")
	})

	t.Run("unknown match mode is an error listing the accepted set", func(t *testing.T) {
		_, err := NewVolumeKeyRewriter(nil, VolumeMatchMode("prefix"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "prefix")
		assert.Contains(t, err.Error(), "suffix")
	})

	t.Run("no error silently falls back to the defaults", func(t *testing.T) {
		rw, err := NewVolumeKeyRewriter([]VolumeKeyRule{{Pattern: "([", Replacement: "x"}}, "")
		require.Error(t, err)
		assert.Nil(t, rw, "a rejected configuration yields no usable rewriter")
	})
}

func TestVolumeKeyRewriter_MatchModes(t *testing.T) {
	const token = "pvc_9f3a"
	cases := []struct {
		mode   VolumeMatchMode
		volume string
		want   bool
	}{
		{VolumeMatchExact, "pvc_9f3a", true},
		{VolumeMatchExact, "trident_pvc_9f3a", false},
		{VolumeMatchSuffix, "trident_pvc_9f3a", true},
		{VolumeMatchSuffix, "pvc_9f3a", true},
		{VolumeMatchSuffix, "trident_pvc_9f3a_clone", false},
		{VolumeMatchContains, "trident_pvc_9f3a_clone", true},
		{VolumeMatchContains, "trident_pvc_0000", false},
		{VolumeMatchRegex, "trident_pvc_9f3a", true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%s", c.mode, c.volume), func(t *testing.T) {
			rw := mustRewriter(t, nil, c.mode)
			assert.Equal(t, c.want, rw.matches(token, c.volume))
		})
	}

	t.Run("an empty token never matches", func(t *testing.T) {
		for _, m := range VolumeMatchModes {
			assert.False(t, mustRewriter(t, nil, m).matches("", "anything"), string(m))
		}
	})
}

// Pins the spec scenario "Suffix mode rejects a clone whose name extends past
// the PV name": the default mode must resolve the FlexVol without knowing the
// provisioner's storage prefix while still excluding a derived volume.
func TestVolumeMatcher_SuffixRejectsClone(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}}
	m := newVolumeMatcher(mustRewriter(t, nil, ""), claims)

	assert.True(t, m.any("trident_pvc_9f3a"))
	assert.False(t, m.any("trident_pvc_9f3a_clone"))
	assert.Equal(t, []int{0}, m.match("trident_pvc_9f3a", nil))
	assert.Empty(t, m.match("trident_pvc_9f3a_clone", nil))
}

// Pins "Contains mode admits what suffix mode rejects".
func TestVolumeMatcher_ContainsAdmitsClone(t *testing.T) {
	claims := []pvcVolume{{id: "c/db/data", volumeName: "pvc-9f3a"}}
	m := newVolumeMatcher(mustRewriter(t, nil, VolumeMatchContains), claims)

	assert.True(t, m.any("trident_pvc_9f3a"))
	assert.True(t, m.any("trident_pvc_9f3a_clone"))
}

func TestVolumeMatcher_RegexTokenThatDoesNotCompileNeverMatches(t *testing.T) {
	// The token comes from upstream data, not configuration, so a PV name that
	// is not valid regex degrades to "no match" instead of failing the build.
	claims := []pvcVolume{
		{id: "c/db/bad", volumeName: "pvc-(["},
		{id: "c/db/good", volumeName: "pvc-9f3a"},
	}
	m := newVolumeMatcher(mustRewriter(t, nil, VolumeMatchRegex), claims)

	assert.Empty(t, m.match("pvc_([", nil))
	assert.Equal(t, []int{1}, m.match("trident_pvc_9f3a", nil))
}

// The bucketed exact/suffix index and the linear scan must agree exactly: the
// index is an optimisation, never a different predicate.
func TestVolumeMatcher_IndexAgreesWithScan(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260901))
	seg := func() string {
		const hex = "0123456789abcdef"
		b := make([]byte, 1+rnd.Intn(6))
		for i := range b {
			b[i] = hex[rnd.Intn(len(hex))]
		}
		return string(b)
	}

	claims := make([]pvcVolume, 0, 40)
	for i := range 40 {
		claims = append(claims, pvcVolume{
			id:         fmt.Sprintf("c/ns/claim-%d", i),
			volumeName: "pvc-" + seg() + "-" + seg(),
		})
	}

	volumes := make([]string, 0, 120)
	for _, c := range claims {
		u := strings.ReplaceAll(c.volumeName, "-", "_")
		volumes = append(volumes, u, "trident_"+u, "trident_"+u+"_clone", u+seg())
	}
	for range 40 {
		volumes = append(volumes, "unrelated_"+seg())
	}

	for _, mode := range []VolumeMatchMode{VolumeMatchExact, VolumeMatchSuffix} {
		t.Run(string(mode), func(t *testing.T) {
			rw := mustRewriter(t, nil, mode)
			m := newVolumeMatcher(rw, claims)
			for _, v := range volumes {
				want := []int{}
				for i, c := range claims {
					if rw.matches(rw.token(c.volumeName), v) {
						want = append(want, i)
					}
				}
				got := append([]int{}, m.match(v, nil)...)
				sort.Ints(got)
				assert.Equal(t, want, got, "volume %q", v)
				assert.Equal(t, len(want) > 0, m.any(v), "volume %q", v)
			}
		})
	}
}

func pvcInfoSeries(volumeNames ...string) model.Vector {
	out := make(model.Vector, 0, len(volumeNames))
	for i, vn := range volumeNames {
		metric := model.Metric{
			"cluster":               "c1",
			"namespace":             "db",
			"persistentvolumeclaim": model.LabelValue(fmt.Sprintf("claim-%d", i)),
		}
		if vn != "" {
			metric["volumename"] = model.LabelValue(vn)
		}
		out = append(out, &model.Sample{Metric: metric, Value: 1})
	}
	return out
}

func volumeLabelSeries(volumes ...string) model.Vector {
	out := make(model.Vector, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, &model.Sample{Metric: model.Metric{
			"volume":  model.LabelValue(v),
			"cluster": "ontap-prod",
			"aggr":    "aggr1",
			"node":    "ontap-prod-01",
		}, Value: 1})
	}
	return out
}

func TestQoSVolumeScope(t *testing.T) {
	rw := mustRewriter(t, nil, "")

	t.Run("returns the matched FlexVol names sorted and deduped", func(t *testing.T) {
		scope := qosVolumeScope(
			pvcInfoSeries("pvc-b", "pvc-a"),
			volumeLabelSeries("trident_pvc_b", "trident_pvc_a", "trident_pvc_a", "unrelated_vol"),
			rw)
		assert.Equal(t, []string{"trident_pvc_a", "trident_pvc_b"}, scope)
	})

	t.Run("no match yields an empty scope", func(t *testing.T) {
		assert.Empty(t, qosVolumeScope(
			pvcInfoSeries("pvc-a"), volumeLabelSeries("vol0", "root_vol"), rw))
	})

	t.Run("empty inputs yield an empty scope", func(t *testing.T) {
		assert.Empty(t, qosVolumeScope(nil, volumeLabelSeries("trident_pvc_a"), rw))
		assert.Empty(t, qosVolumeScope(pvcInfoSeries("pvc-a"), nil, rw))
	})

	t.Run("a PVC carrying no volumename contributes nothing", func(t *testing.T) {
		assert.Empty(t, qosVolumeScope(
			pvcInfoSeries("", ""), volumeLabelSeries("trident_pvc_a"), rw))
	})

	t.Run("scope is a superset covering unbound claims", func(t *testing.T) {
		// pvc-c has no pod binding and will never become a PVC entity, but its
		// name is still read off the raw info vector: the scope widens what is
		// fetched, never what joins.
		scope := qosVolumeScope(
			pvcInfoSeries("pvc-a", "pvc-c"),
			volumeLabelSeries("trident_pvc_a", "trident_pvc_c"), rw)
		assert.Equal(t, []string{"trident_pvc_a", "trident_pvc_c"}, scope)
	})

	t.Run("input order does not change the scope", func(t *testing.T) {
		a := qosVolumeScope(pvcInfoSeries("pvc-a", "pvc-b"),
			volumeLabelSeries("trident_pvc_b", "trident_pvc_a"), rw)
		b := qosVolumeScope(pvcInfoSeries("pvc-b", "pvc-a"),
			volumeLabelSeries("trident_pvc_a", "trident_pvc_b"), rw)
		assert.Equal(t, a, b)
	})
}

// A zero Options — an embedder that configures nothing, and every existing
// test — resolves the provisioner-agnostic defaults rather than a nil rewriter.
func TestOptions_VolumeKeyDefaults(t *testing.T) {
	var opts Options
	rw := opts.volumeKey()
	require.NotNil(t, rw)
	assert.Equal(t, DefaultVolumeMatchMode, rw.mode)
	assert.Equal(t, "pvc_9f3a", rw.token("pvc-9f3a"))
	assert.Equal(t, DefaultQoSScopeBatchBytes, opts.qosScopeBatchBytes())

	custom, err := NewVolumeKeyRewriter([]VolumeKeyRule{{Pattern: "-", Replacement: "."}}, VolumeMatchExact)
	require.NoError(t, err)
	set := Options{VolumeKey: custom, QoSScopeBatchBytes: 512}
	assert.Same(t, custom, set.volumeKey())
	assert.Equal(t, 512, set.qosScopeBatchBytes())

	assert.Equal(t, DefaultQoSScopeBatchBytes, Options{QoSScopeBatchBytes: -1}.qosScopeBatchBytes(),
		"a negative budget is not a smaller budget")
}
