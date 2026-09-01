package build

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// scopeFake answers the two prerequisite legs with claim/volume fixtures and
// every QoS workload query from a per-volume table, recording what it was asked.
type scopeFake struct {
	mu      sync.Mutex
	queries map[string][]string
	order   []string

	claims  model.Vector
	volumes model.Vector
	// readOps maps a FlexVol name to its qos_read_ops value.
	readOps map[string]float64
	// failVolume makes any query whose alternation names it fail.
	failVolume string
	// delay is applied to a query naming delayVolume, forcing out-of-order
	// completion without making the test depend on the scheduler.
	delayVolume string
	delay       time.Duration
}

func (f *scopeFake) Instant(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
	f.mu.Lock()
	f.queries[name] = append(f.queries[name], query)
	f.order = append(f.order, name)
	f.mu.Unlock()

	switch promql.Query(name) {
	case promql.QPVCInfo:
		return f.claims, nil
	case promql.QVolumeLabels:
		return f.volumes, nil
	default:
	}
	if !isQoSWorkload(promql.Query(name)) {
		return model.Vector{}, nil
	}
	if f.delayVolume != "" && strings.Contains(query, f.delayVolume) {
		time.Sleep(f.delay)
	}
	if f.failVolume != "" && strings.Contains(query, f.failVolume) {
		return nil, errors.New("upstream 5xx")
	}
	if promql.Query(name) != promql.QQoSReadOps {
		return model.Vector{}, nil
	}
	var out model.Vector
	for vol, val := range f.readOps {
		if !strings.Contains(query, vol) {
			continue
		}
		out = append(out, &model.Sample{Metric: model.Metric{
			"volume": model.LabelValue(vol), "cluster": "ontap-prod", "svm": "svm0",
		}, Value: model.SampleValue(val)})
	}
	return out, nil
}

func (f *scopeFake) queriesFor(q promql.Query) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries[string(q)]...)
}

func isQoSWorkload(q promql.Query) bool {
	return slices.Contains(promql.QoSWorkloadQueries, q)
}

func claimSample(ns, claim, pv string) *model.Sample {
	return &model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": model.LabelValue(ns),
		"persistentvolumeclaim": model.LabelValue(claim),
		"volumename":            model.LabelValue(pv),
	}, Value: 1}
}

func volSample(vol, aggr string) *model.Sample {
	return &model.Sample{Metric: model.Metric{
		"volume": model.LabelValue(vol), "cluster": "ontap-prod",
		"node": "ontap-prod-01", "aggr": model.LabelValue(aggr), "svm": "svm0",
	}, Value: 1}
}

func newScopeFake() *scopeFake {
	return &scopeFake{queries: map[string][]string{}, readOps: map[string]float64{}}
}

func readTopologyWith(t *testing.T, q promql.Querier, opts Options) {
	t.Helper()
	_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(),
		opts, promql.Selector{})
	require.NoError(t, err)
}

// scopedQoSVectors drives readScopedQoS directly with both prerequisites
// already satisfied, so a test can assert on the merged vectors themselves
// rather than on what survived the parse.
func scopedQoSVectors(t *testing.T, q promql.Querier, opts Options, v *topologyVectors) {
	t.Helper()
	ctx := context.Background()
	done := func() <-chan struct{} {
		c := make(chan struct{})
		close(c)
		return c
	}
	require.NoError(t, readScopedQoS(ctx, ctx, q, time.Minute, time.Unix(1, 0).UTC(),
		opts, v, done(), done()))
}

// The QoS read waits for the two legs its scope is computed from, and the query
// it then issues names exactly the FlexVol names those claims matched.
func TestReadScopedQoS_RestrictedToMatchedVolumes(t *testing.T) {
	f := newScopeFake()
	f.claims = model.Vector{claimSample("db", "data", "pvc-a"), claimSample("db", "other", "pvc-b")}
	f.volumes = model.Vector{
		volSample("trident_pvc_a", "aggr1"),
		volSample("root_vol", "aggr1"), // matches no claim
	}

	readTopologyWith(t, f, Options{})

	got := f.queriesFor(promql.QQoSReadOps)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], `volume="trident_pvc_a"`)
	assert.NotContains(t, got[0], "root_vol", "an unmatched workload is never fetched")
	assert.Contains(t, got[0], `lun=""`, "the fixed granularity contract is composed, not replaced")

	// The prerequisites were asked before the family they gate.
	f.mu.Lock()
	defer f.mu.Unlock()
	var qosAt, pvcAt, volAt = -1, -1, -1
	for i, n := range f.order {
		switch promql.Query(n) {
		case promql.QPVCInfo:
			pvcAt = i
		case promql.QVolumeLabels:
			volAt = i
		case promql.QQoSReadOps:
			if qosAt < 0 {
				qosAt = i
			}
		default:
		}
	}
	assert.Greater(t, qosAt, pvcAt, "the scoped read waits for kube_persistentvolumeclaim_info")
	assert.Greater(t, qosAt, volAt, "the scoped read waits for volume_labels")
}

// Spec: "No matched volumes issues no QoS query" and "Volume-label family
// absent issues no QoS query".
func TestReadScopedQoS_EmptyScopeIssuesNothing(t *testing.T) {
	cases := map[string]func(*scopeFake){
		"no claim matches any volume": func(f *scopeFake) {
			f.claims = model.Vector{claimSample("db", "data", "pvc-a")}
			f.volumes = model.Vector{volSample("root_vol", "aggr1")}
		},
		"volume-label family absent": func(f *scopeFake) {
			f.claims = model.Vector{claimSample("db", "data", "pvc-a")}
			f.volumes = nil
		},
		"no claims at all": func(f *scopeFake) {
			f.volumes = model.Vector{volSample("trident_pvc_a", "aggr1")}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newScopeFake()
			setup(f)

			readTopologyWith(t, f, Options{})

			for _, q := range promql.QoSWorkloadQueries {
				assert.Empty(t, f.queriesFor(q), "%s must not be issued at all", q)
			}
		})
	}
}

// A scope larger than the byte budget is split, and the merge is by chunk
// index — never completion order — so the summed I/O is a pure function of the
// scope even when a later chunk answers first.
func TestReadScopedQoS_ChunksMergeInChunkOrder(t *testing.T) {
	build := func() *scopeFake {
		f := newScopeFake()
		f.claims = model.Vector{
			claimSample("db", "a", "pvc-aaaaaaaa"),
			claimSample("db", "b", "pvc-bbbbbbbb"),
		}
		f.volumes = model.Vector{
			volSample("trident_pvc_aaaaaaaa", "aggr1"),
			volSample("trident_pvc_bbbbbbbb", "aggr1"),
		}
		f.readOps = map[string]float64{
			"trident_pvc_aaaaaaaa": 10,
			"trident_pvc_bbbbbbbb": 20,
		}
		return f
	}

	// A budget below one name's rendered length forces one chunk per name.
	opts := Options{QoSScopeBatchBytes: 21}

	plain := build()
	var vPlain topologyVectors
	vPlain.PVCInfo, vPlain.VolumeLabels = plain.claims, plain.volumes
	scopedQoSVectors(t, plain, opts, &vPlain)
	require.Len(t, plain.queriesFor(promql.QQoSReadOps), 2, "the scope was chunked")

	// Make the FIRST chunk answer last.
	reordered := build()
	reordered.delayVolume = "trident_pvc_aaaaaaaa"
	reordered.delay = 40 * time.Millisecond
	var vReordered topologyVectors
	vReordered.PVCInfo, vReordered.VolumeLabels = reordered.claims, reordered.volumes
	scopedQoSVectors(t, reordered, opts, &vReordered)

	require.Len(t, vPlain.QoSReadOps, 2)
	require.Len(t, vReordered.QoSReadOps, 2)
	assert.Equal(t, vPlain.QoSReadOps[0].Metric["volume"], vReordered.QoSReadOps[0].Metric["volume"],
		"the merged vector follows chunk order, not completion order")
	assert.Equal(t, model.LabelValue("trident_pvc_aaaaaaaa"), vReordered.QoSReadOps[0].Metric["volume"])
}

// One failed chunk costs I/O measurements only for the claims whose volumes it
// carried; every other claim keeps its metrics, and no claim loses its edge,
// aggregate, controller or svm.
func TestReadScopedQoS_FailedChunkDegradesOnlyItsOwnClaims(t *testing.T) {
	f := newScopeFake()
	f.claims = model.Vector{
		claimSample("db", "a", "pvc-aaaaaaaa"),
		claimSample("db", "b", "pvc-bbbbbbbb"),
	}
	f.volumes = model.Vector{
		volSample("trident_pvc_aaaaaaaa", "aggr1"),
		volSample("trident_pvc_bbbbbbbb", "aggr1"),
	}
	f.readOps = map[string]float64{
		"trident_pvc_aaaaaaaa": 10,
		"trident_pvc_bbbbbbbb": 20,
	}
	f.failVolume = "trident_pvc_bbbbbbbb"

	var v topologyVectors
	v.PVCInfo, v.VolumeLabels = f.claims, f.volumes
	scopedQoSVectors(t, f, Options{QoSScopeBatchBytes: 21}, &v)

	require.Len(t, v.QoSReadOps, 1, "only the surviving chunk contributed series")
	assert.Equal(t, model.LabelValue("trident_pvc_aaaaaaaa"), v.QoSReadOps[0].Metric["volume"])
	assert.InDelta(t, 10.0, float64(v.QoSReadOps[0].Value), 1e-12,
		"the surviving claim keeps its measurement")
}

// The topology read still fails when a REQUIRED leg fails, even though the
// scoped QoS read now sits in the same errgroup waiting on a channel.
func TestReadScopedQoS_RequiredLegFailureStillFailsTheBuild(t *testing.T) {
	q := legQuerier(t, failingLegs(nil, errors.New("upstream 5xx"), promql.QPodInfo))

	_, err := ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream 5xx")
}
