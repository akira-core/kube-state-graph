package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// ioRollupSuite is its own VictoriaMetrics so -run IORollup does not start
// GraphSuite, and so this claim's samples cannot meet another test's series.
type ioRollupSuite struct {
	VMSuite
}

func TestIORollup(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(ioRollupSuite))
}

func (s *ioRollupSuite) SetupSuite()    { s.VMSuite.SetupSuite() }
func (s *ioRollupSuite) TearDownSuite() { s.VMSuite.TearDownSuite() }

// TestSVMPVCReadOps imports one joined claim and checks its svm-pvc edge under
// each io_rollup:
//
//   - qos_read_ops is 100, then 400, then 200.
//   - qos_read_latency is 300, 900, 300 at the same instants. Its time mean is
//     500; a mean weighted by read_ops would be 642.857, so avg must report 500
//     (latency under avg is an unweighted time mean).
//   - qos_write_ops has samples only in the last quarter of the window, 60 then
//     100. avg must report 80, the mean of the samples the series has; a mean
//     that counted the rest of the window as zero would be far lower.
//
// The request window is [fixedNow-5m, fixedNow]. fixedNow sits on the 30s
// end-align grid, so AlignWindow does not move it. The range selector is
// (start, end]. Every sample is strictly inside that interval. Nothing is
// imported at or before start, so a rollup that pulled a pre-window point would
// have no sample to pull.
func (s *ioRollupSuite) TestSVMPVCReadOps() {
	s.Zero(fixedNow.Unix()%30, "fixedNow must stay on the default end-align grid")

	disc := s.T().Name()
	at := s.inWindow()
	t100 := at(-4 * time.Minute)
	t400 := at(-2 * time.Minute)
	t200 := at(-time.Minute)
	tw60 := at(-60 * time.Second) // last quarter of a 5m window starts at -75s
	tw100 := at(-30 * time.Second)
	tEnd := fixedNow.Unix() * 1000

	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="iorollup",pod="io-0",uid="uid-io-0",node="worker-io",az="zone-a",env="prod",test=%[1]q} 1 %[5]d
kube_node_info{cluster="c1",node="worker-io",az="zone-a",env="prod",test=%[1]q} 1 %[5]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iorollup",persistentvolumeclaim="io-data",storageclass="netapp-nas",volumename="pvc-io-roll",az="zone-a",env="prod",test=%[1]q} 1 %[5]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iorollup",pod="io-0",persistentvolumeclaim="io-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[5]d
volume_labels{az="zone-a",env="prod",cluster="ontap-io",node="ontap-io-01",aggr="io-aggr1",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 1 %[5]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 100 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 400 %[3]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 200 %[4]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 300 %[2]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 900 %[3]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 300 %[4]d
qos_write_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 60 %[6]d
qos_write_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io",volume="trident_pvc_io_roll",test=%[1]q} 100 %[7]d
`, disc, t100, t400, t200, tEnd, tw60, tw100))
	s.waitForAll(disc,
		`volume_labels{volume="trident_pvc_io_roll"`,
		`qos_read_ops{volume="trident_pvc_io_roll"`,
		`qos_read_latency{volume="trident_pvc_io_roll"`,
		`qos_write_ops{volume="trident_pvc_io_roll"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="io-0"`,
	)

	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.EndAlign = 30 * time.Second
	})
	defer srv.Close()

	for _, tc := range []struct {
		rollup                       string
		readOps, readLatUs, writeOps float64
	}{
		{rollup: "last", readOps: 200, readLatUs: 300, writeOps: 100},
		{rollup: "max", readOps: 400, readLatUs: 900, writeOps: 100},
		{rollup: "avg", readOps: 233.333, readLatUs: 500, writeOps: 80},
	} {
		body := s.fetchIORollup(srv.URL, "io-aggr1", tc.rollup)
		edges := tierEdges(body, graph.StorageTierSVMPVC)
		s.Require().Len(edges, 1, "one svm-pvc edge under io_rollup=%s", tc.rollup)
		m := edges[0].Data.Metrics
		s.Require().NotNil(m, "io_rollup=%s", tc.rollup)
		s.Require().NotNil(m.ReadOps, "io_rollup=%s", tc.rollup)
		s.Require().NotNil(m.ReadLatencyUs, "io_rollup=%s", tc.rollup)
		s.Require().NotNil(m.WriteOps, "io_rollup=%s", tc.rollup)
		s.InDelta(tc.readOps, *m.ReadOps, 1e-9, "read_ops, io_rollup=%s", tc.rollup)
		s.InDelta(tc.readLatUs, *m.ReadLatencyUs, 1e-9, "read_latency_us, io_rollup=%s", tc.rollup)
		s.InDelta(tc.writeOps, *m.WriteOps, 1e-9, "write_ops, io_rollup=%s", tc.rollup)
	}
}

// TestTierSumsPerClaimPeaks puts two claims on one aggregate and SVM whose
// qos_read_ops peak at different instants: claim a is 400 then 100, claim b
// is 100 then 300. At no instant does the aggregate carry more than 500, but
// under max the aggr-svm hop carries 700 — the sum of the claims' own peaks.
// Under last it carries 400 (100 + 300) and under avg 450 (250 + 200).
func (s *ioRollupSuite) TestTierSumsPerClaimPeaks() {
	disc := s.T().Name()
	at := s.inWindow()
	tEarly := at(-4 * time.Minute)
	tLate := at(-time.Minute)
	tEnd := fixedNow.Unix() * 1000

	s.IngestExpFmt(fmt.Sprintf(`
kube_node_info{cluster="c1",node="worker-io2",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_pod_info{cluster="c1",namespace="iorollup",pod="io-a-0",uid="uid-io-a-0",node="worker-io2",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_pod_info{cluster="c1",namespace="iorollup",pod="io-b-0",uid="uid-io-b-0",node="worker-io2",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iorollup",persistentvolumeclaim="io-a-data",storageclass="netapp-nas",volumename="pvc-io-a",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iorollup",persistentvolumeclaim="io-b-data",storageclass="netapp-nas",volumename="pvc-io-b",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iorollup",pod="io-a-0",persistentvolumeclaim="io-a-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iorollup",pod="io-b-0",persistentvolumeclaim="io-b-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[4]d
volume_labels{az="zone-a",env="prod",cluster="ontap-io",node="ontap-io-01",aggr="io-aggr2",svm="svm_io2",volume="trident_pvc_io_a",test=%[1]q} 1 %[4]d
volume_labels{az="zone-a",env="prod",cluster="ontap-io",node="ontap-io-01",aggr="io-aggr2",svm="svm_io2",volume="trident_pvc_io_b",test=%[1]q} 1 %[4]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io2",volume="trident_pvc_io_a",test=%[1]q} 400 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io2",volume="trident_pvc_io_a",test=%[1]q} 100 %[3]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io2",volume="trident_pvc_io_b",test=%[1]q} 100 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-io",svm="svm_io2",volume="trident_pvc_io_b",test=%[1]q} 300 %[3]d
`, disc, tEarly, tLate, tEnd))
	s.waitForAll(disc,
		`volume_labels{volume="trident_pvc_io_a"`,
		`volume_labels{volume="trident_pvc_io_b"`,
		`qos_read_ops{volume="trident_pvc_io_a"`,
		`qos_read_ops{volume="trident_pvc_io_b"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="io-a-0"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="io-b-0"`,
	)

	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.EndAlign = 30 * time.Second
	})
	defer srv.Close()

	for _, tc := range []struct {
		rollup  string
		readOps float64
	}{
		{rollup: "last", readOps: 400},
		{rollup: "max", readOps: 700},
		{rollup: "avg", readOps: 450},
	} {
		body := s.fetchIORollup(srv.URL, "io-aggr2", tc.rollup)
		s.Len(tierEdges(body, graph.StorageTierSVMPVC), 2, "both claims flow through the hops")
		for _, tier := range []string{graph.StorageTierNodeAggr, graph.StorageTierAggrSVM} {
			edges := tierEdges(body, tier)
			s.Require().Len(edges, 1, "one %s edge under io_rollup=%s", tier, tc.rollup)
			m := edges[0].Data.Metrics
			s.Require().NotNil(m, "%s, io_rollup=%s", tier, tc.rollup)
			s.Require().NotNil(m.ReadOps, "%s, io_rollup=%s", tier, tc.rollup)
			s.InDelta(tc.readOps, *m.ReadOps, 1e-9, "%s, io_rollup=%s", tier, tc.rollup)
		}
	}
}

// inWindow returns a converter from an offset before fixedNow to a sample
// timestamp in milliseconds, failing the test for an offset outside
// (fixedNow-5m, fixedNow] — the request window every test here sends.
func (s *ioRollupSuite) inWindow() func(time.Duration) int64 {
	start := fixedNow.Add(-5 * time.Minute)
	return func(offset time.Duration) int64 {
		ts := fixedNow.Add(offset)
		s.True(ts.After(start) && !ts.After(fixedNow), "sample %s is outside (start, end]", offset)
		return ts.Unix() * 1000
	}
}

// waitForAll waits until VictoriaMetrics serves each series. Every prefix is
// a selector missing only its closing `,test="<disc>"}`.
func (s *ioRollupSuite) waitForAll(disc string, prefixes ...string) {
	s.T().Helper()
	for _, p := range prefixes {
		series := p + `,test=` + strconv.Quote(disc) + `}`
		s.Require().True(s.WaitForSeries(series, fixedNow, 30*time.Second), "VM did not observe %s", series)
	}
}

// tierEdges returns the storage-flow edges of one tier.
func tierEdges(body cytoscape.Body, tier string) []cytoscape.Edge {
	var out []cytoscape.Edge
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] == tier {
			out = append(out, e)
		}
	}
	return out
}

func (s *ioRollupSuite) fetchIORollup(base, aggr, rollup string) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.storageGraphURL(base, func(q url.Values) {
		q.Set("aggr", aggr)
		q.Set("io_rollup", rollup)
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode, "io_rollup=%s", rollup)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}
