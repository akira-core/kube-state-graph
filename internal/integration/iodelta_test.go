package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/suite"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// ioDeltaSuite is its own VictoriaMetrics so -run IODelta starts nothing else
// and this claim's samples cannot meet another test's series.
type ioDeltaSuite struct {
	VMSuite
}

func TestIODelta(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(ioDeltaSuite))
}

func (s *ioDeltaSuite) SetupSuite()    { s.VMSuite.SetupSuite() }
func (s *ioDeltaSuite) TearDownSuite() { s.VMSuite.TearDownSuite() }

// TestEndMinusStart imports two claims on one aggregate and SVM and checks the
// io_delta=true body. The request window is [fixedNow-5m, fixedNow]; fixedNow
// sits on the 30s end-align grid, so AlignWindow does not move it and the
// samples below are placed relative to the window AFTER flooring. The delta
// reads the last sample in (end-5m, end] minus the last sample in
// (end-10m, end-5m]; no sample sits on a boundary.
//
//   - Claim a: qos_read_ops 50 (-7m) then 100 (-1m) -> read_ops_delta 50.
//     qos_write_data 4000000 (-8m) then 1000000 (-30s) -> write_bytes_per_sec_delta
//     -3000000. qos_read_latency 400 (-6m) then 650 (-1m) -> read_latency_us_delta
//     250. qos_write_ops has a sample inside the window only, so no
//     write_ops_delta.
//   - Claim b was created inside the window: it carries its measurement and no
//     change field, so the node-aggr and aggr-svm edges carry a's change only.
//
// Then the two-request oracle: the same figure under io_rollup=last for the
// window of the same length ending at the first request's start, subtracted
// from the first request's, equals the change.
func (s *ioDeltaSuite) TestEndMinusStart() {
	s.Zero(fixedNow.Unix()%30, "fixedNow must stay on the default end-align grid")

	disc := s.T().Name()
	ms := func(offset time.Duration) int64 { return fixedNow.Add(offset).Unix() * 1000 }
	tEnd := fixedNow.Unix() * 1000

	topology := func(ts int64) string {
		return fmt.Sprintf(`
kube_node_info{cluster="c1",node="worker-iod",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="iodelta",pod="iod-a-0",uid="uid-iod-a-0",node="worker-iod",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="iodelta",pod="iod-b-0",uid="uid-iod-b-0",node="worker-iod",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iodelta",persistentvolumeclaim="iod-a-data",storageclass="netapp-nas",volumename="pvc-iod-a",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iodelta",persistentvolumeclaim="iod-b-data",storageclass="netapp-nas",volumename="pvc-iod-b",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iodelta",pod="iod-a-0",persistentvolumeclaim="iod-a-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iodelta",pod="iod-b-0",persistentvolumeclaim="iod-b-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-iod",node="ontap-iod-01",aggr="iod-aggr1",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-iod",node="ontap-iod-01",aggr="iod-aggr1",svm="svm_iod",volume="trident_pvc_iod_b",test=%[1]q} 1 %[2]d
`, disc, ts)
	}
	s.IngestExpFmt(topology(tEnd) + topology(ms(-7*time.Minute)) + fmt.Sprintf(`
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 50 %[3]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 100 %[4]d
qos_write_ops{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 20 %[4]d
qos_write_data{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 4000000 %[5]d
qos_write_data{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 1000000 %[6]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 400 %[7]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_a",test=%[1]q} 650 %[4]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_b",test=%[1]q} 70 %[4]d
qos_read_latency{az="zone-a",env="prod",cluster="ontap-iod",svm="svm_iod",volume="trident_pvc_iod_b",test=%[1]q} 90 %[4]d
`, disc, tEnd, ms(-7*time.Minute), ms(-time.Minute), ms(-8*time.Minute), ms(-30*time.Second), ms(-6*time.Minute)))

	for _, series := range []string{
		`volume_labels{volume="trident_pvc_iod_a"`,
		`volume_labels{volume="trident_pvc_iod_b"`,
		`qos_read_ops{volume="trident_pvc_iod_a"`,
		`qos_read_ops{volume="trident_pvc_iod_b"`,
		`qos_write_data{volume="trident_pvc_iod_a"`,
		`qos_read_latency{volume="trident_pvc_iod_a"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="iod-b-0"`,
	} {
		q := series + `,test=` + strconv.Quote(disc) + `}`
		s.Require().True(s.WaitForSeries(q, fixedNow, 30*time.Second), "VM did not observe %s", q)
	}
	// The pre-start samples are visible only to an evaluation inside their own span.
	for _, series := range []string{
		`qos_read_ops{volume="trident_pvc_iod_a"`,
		`qos_write_data{volume="trident_pvc_iod_a"`,
		`qos_read_latency{volume="trident_pvc_iod_a"`,
	} {
		q := series + `,test=` + strconv.Quote(disc) + `}`
		s.Require().True(s.WaitForSeries(q, fixedNow.Add(-5*time.Minute), 30*time.Second), "VM did not observe pre-start %s", q)
	}

	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.EndAlign = 30 * time.Second
	})
	defer srv.Close()

	body := s.fetchIODelta(srv.URL, "iod-aggr1", "true", nil)
	claims := claimEdgesByName(body)
	a, b := claims["iod-a-data"], claims["iod-b-data"]
	s.Require().NotNil(a, "claim a edge")
	s.Require().NotNil(b, "claim b edge")

	s.Require().NotNil(a.ReadOps)
	s.InDelta(100, *a.ReadOps, 1e-9)
	s.Require().NotNil(a.ReadOpsDelta, "read_ops_delta")
	s.InDelta(50, *a.ReadOpsDelta, 1e-9)
	s.Require().NotNil(a.WriteBytesPerSecDelta, "write_bytes_per_sec_delta")
	s.InDelta(-3_000_000, *a.WriteBytesPerSecDelta, 1e-6)
	s.Require().NotNil(a.WriteBytesPerSec)
	s.InDelta(1_000_000, *a.WriteBytesPerSec, 1e-6)
	s.Require().NotNil(a.ReadLatencyUsDelta, "read_latency_us_delta on svm-pvc")
	s.InDelta(250, *a.ReadLatencyUsDelta, 1e-9)
	s.Require().NotNil(a.WriteOps)
	s.Nil(a.WriteOpsDelta, "write_ops has no start value: absent, never 0")

	s.Require().NotNil(b.ReadOps, "claim b keeps its measurement")
	s.InDelta(70, *b.ReadOps, 1e-9)
	s.Nil(b.ReadOpsDelta, "claim b was created inside the window")
	s.Nil(b.ReadLatencyUsDelta)

	for _, tier := range []string{graph.StorageTierNodeAggr, graph.StorageTierAggrSVM} {
		edges := tierEdges(body, tier)
		s.Require().Len(edges, 1, tier)
		m := edges[0].Data.Metrics
		s.Require().NotNil(m, tier)
		s.Require().NotNil(m.ReadOps, tier)
		s.InDelta(170, *m.ReadOps, 1e-9, "%s sums both claims' measurements", tier)
		s.Require().NotNil(m.ReadOpsDelta, tier)
		s.InDelta(50, *m.ReadOpsDelta, 1e-9, "%s carries claim a's change only", tier)
		s.Nil(m.ReadLatencyUsDelta, "%s: latency change is claim-level only", tier)
	}

	// Without io_delta nothing changes and no change field appears.
	plain := s.fetchIODelta(srv.URL, "iod-aggr1", "", nil)
	for _, e := range plain.Elements.Edges {
		raw, err := json.Marshal(e.Data.Metrics)
		s.Require().NoError(err)
		s.NotContains(string(raw), "_delta")
	}

	// Independent of the rollup: under max the edge carries the peak beside the
	// end-to-start change.
	maxed := s.fetchIODelta(srv.URL, "iod-aggr1", "true", func(q url.Values) { q.Set("io_rollup", "max") })
	am := claimEdgesByName(maxed)["iod-a-data"]
	s.Require().NotNil(am)
	s.Require().NotNil(am.ReadOpsDelta)
	s.InDelta(50, *am.ReadOpsDelta, 1e-9)

	// The two-request oracle: delta == last(window) - last(window shifted back
	// by its own length). 5m is a multiple of the end-align step, so both
	// windows stay on the grid.
	start, end := fixedNow.Add(-5*time.Minute), fixedNow
	earlier := s.fetchIODelta(srv.URL, "iod-aggr1", "", func(q url.Values) {
		q.Set("start", strconv.FormatInt(start.Add(-5*time.Minute).Unix(), 10))
		q.Set("end", strconv.FormatInt(start.Unix(), 10))
	})
	later := s.fetchIODelta(srv.URL, "iod-aggr1", "", func(q url.Values) {
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
	})
	e0, l0 := claimEdgesByName(earlier)["iod-a-data"], claimEdgesByName(later)["iod-a-data"]
	s.Require().NotNil(e0, "claim a under the earlier window")
	s.Require().NotNil(l0)
	s.InDelta(*l0.ReadOps-*e0.ReadOps, *a.ReadOpsDelta, 1e-9, "read_ops_delta == last - last(shifted)")
	s.InDelta(*l0.WriteBytesPerSec-*e0.WriteBytesPerSec, *a.WriteBytesPerSecDelta, 1e-6)
	s.InDelta(*l0.ReadLatencyUs-*e0.ReadLatencyUs, *a.ReadLatencyUsDelta, 1e-9)
}

// TestRelabelInsideTheWindowLeavesNoDelta proves against a real store that a
// workload whose labels change inside the window carries no change field. Its
// own aggregate, SVM, controller, node, namespace, claims and FlexVols keep it
// apart from TestEndMinusStart's sums. The window is [fixedNow-5m, fixedNow].
//
//   - Claim r: qos_read_ops policy_group="silver" 40 (-7m) then 110 (-3m), then
//     the workload moves to policy_group="gold": 300 (-1m). The old silver
//     series has a sample on both sides of start, so the delta expression
//     returns its stale change 70 and nothing for gold (no start sample).
//   - Claim s, same aggregate and SVM: one unchanged series 10 (-7m) then 35
//     (-1m) -> read_ops_delta 25.
//
// First the delta expression goes to VictoriaMetrics directly, showing the
// store does return the stale silver row; then the API body shows the reader
// drops it, so r keeps its measurement and carries no read_ops_delta while s
// keeps its change. r's measurement value is not asserted.
func (s *ioDeltaSuite) TestRelabelInsideTheWindowLeavesNoDelta() {
	s.Zero(fixedNow.Unix()%30, "fixedNow must stay on the default end-align grid")

	disc := s.T().Name()
	ms := func(offset time.Duration) int64 { return fixedNow.Add(offset).Unix() * 1000 }
	tEnd := fixedNow.Unix() * 1000
	const volR, volS = "trident_pvc_iod_r", "trident_pvc_iod_s"

	s.IngestExpFmt(fmt.Sprintf(`
kube_node_info{cluster="c1",node="worker-iod2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="iodelta2",pod="iod-r-0",uid="uid-iod-r-0",node="worker-iod2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="iodelta2",pod="iod-s-0",uid="uid-iod-s-0",node="worker-iod2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iodelta2",persistentvolumeclaim="iod-r-data",storageclass="netapp-nas",volumename="pvc-iod-r",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="iodelta2",persistentvolumeclaim="iod-s-data",storageclass="netapp-nas",volumename="pvc-iod-s",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iodelta2",pod="iod-r-0",persistentvolumeclaim="iod-r-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="iodelta2",pod="iod-s-0",persistentvolumeclaim="iod-s-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-iod2",node="ontap-iod2-01",aggr="iod-aggr2",svm="svm_iod2",volume=%[3]q,test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-iod2",node="ontap-iod2-01",aggr="iod-aggr2",svm="svm_iod2",volume=%[4]q,test=%[1]q} 1 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod2",svm="svm_iod2",volume=%[3]q,policy_group="silver",test=%[1]q} 40 %[5]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod2",svm="svm_iod2",volume=%[3]q,policy_group="silver",test=%[1]q} 110 %[6]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod2",svm="svm_iod2",volume=%[3]q,policy_group="gold",test=%[1]q} 300 %[7]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod2",svm="svm_iod2",volume=%[4]q,policy_group="silver",test=%[1]q} 10 %[5]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-iod2",svm="svm_iod2",volume=%[4]q,policy_group="silver",test=%[1]q} 35 %[7]d
`, disc, tEnd, volR, volS, ms(-7*time.Minute), ms(-3*time.Minute), ms(-time.Minute)))

	// The gate fires only when the API's measurement read sees BOTH of r's
	// series, so each one is awaited by its own policy_group.
	for _, series := range []string{
		`volume_labels{volume="` + volR + `"`,
		`volume_labels{volume="` + volS + `"`,
		`qos_read_ops{volume="` + volR + `",policy_group="silver"`,
		`qos_read_ops{volume="` + volR + `",policy_group="gold"`,
		`qos_read_ops{volume="` + volS + `"`,
		`kube_persistentvolumeclaim_info{persistentvolumeclaim="iod-r-data"`,
		`kube_persistentvolumeclaim_info{persistentvolumeclaim="iod-s-data"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="iod-r-0"`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="iod-s-0"`,
	} {
		q := series + `,test=` + strconv.Quote(disc) + `}`
		s.Require().True(s.WaitForSeries(q, fixedNow, 30*time.Second), "VM did not observe %s", q)
	}
	// The pre-start samples are visible only to an evaluation inside their own span.
	for _, series := range []string{
		`qos_read_ops{volume="` + volR + `",policy_group="silver"`,
		`qos_read_ops{volume="` + volS + `"`,
	} {
		q := series + `,test=` + strconv.Quote(disc) + `}`
		s.Require().True(s.WaitForSeries(q, fixedNow.Add(-5*time.Minute), 30*time.Second), "VM did not observe pre-start %s", q)
	}

	// (1) The store returns the stale change of the old series: exactly one
	// row for r, the silver one, valued silver(-3m) - silver(-7m).
	expr, ok := promql.RenderQoSVolumeScopedDelta(promql.QQoSReadOps, 5*time.Minute,
		promql.LabelKeys{}, promql.Selector{}, []string{volR})
	s.Require().True(ok)
	v := url.Values{}
	v.Set("query", expr)
	v.Set("time", strconv.FormatInt(fixedNow.Unix(), 10))
	v.Set("nocache", "1")
	resp, err := s.vmGet(s.VMURL() + "/api/v1/query?" + v.Encode())
	s.Require().NoError(err)
	var raw struct {
		Status string `json:"status"`
		Data   struct {
			Result model.Vector `json:"result"`
		} `json:"data"`
	}
	rawBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().NoError(readErr)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "VictoriaMetrics answered: %s", rawBody)
	s.Require().NoError(json.Unmarshal(rawBody, &raw), "VictoriaMetrics answered: %s", rawBody)
	s.Require().Equal("success", raw.Status)
	s.Require().Len(raw.Data.Result, 1, "upstream delta rows for %s: %v", volR, raw.Data.Result)
	row := raw.Data.Result[0]
	s.T().Logf("raw upstream delta row: %s => %v", row.Metric, row.Value)
	s.Equal(model.LabelValue("silver"), row.Metric["policy_group"], "the stale row is the old series")
	s.Equal(model.LabelValue(volR), row.Metric[promql.HarvestVolumeLabel])
	s.InDelta(70, float64(row.Value), 1e-9, "silver(-3m) - silver(-7m)")

	// (2) Through the API the reader drops r's delta and keeps s's.
	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.EndAlign = 30 * time.Second
	})
	defer srv.Close()

	body := s.fetchIODelta(srv.URL, "iod-aggr2", "true", nil)
	claims := claimEdgesByName(body)
	r, ctl := claims["iod-r-data"], claims["iod-s-data"]
	s.Require().NotNil(r, "claim r edge")
	s.Require().NotNil(ctl, "claim s edge")

	s.Require().NotNil(r.ReadOps, "claim r keeps its measurement")
	s.Nil(r.ReadOpsDelta, "a relabel inside the window leaves no read_ops_delta")

	s.Require().NotNil(ctl.ReadOps)
	s.InDelta(35, *ctl.ReadOps, 1e-9)
	s.Require().NotNil(ctl.ReadOpsDelta, "the unchanged control keeps its read_ops_delta")
	s.InDelta(25, *ctl.ReadOpsDelta, 1e-9)

	for _, tier := range []string{graph.StorageTierNodeAggr, graph.StorageTierAggrSVM} {
		edges := tierEdges(body, tier)
		s.Require().Len(edges, 1, tier)
		m := edges[0].Data.Metrics
		s.Require().NotNil(m, tier)
		s.Require().NotNil(m.ReadOpsDelta, tier)
		s.InDelta(25, *m.ReadOpsDelta, 1e-9, "%s carries claim s's change only", tier)
	}
}

// claimEdgesByName indexes the svm-pvc edges' metrics by the claim name at the
// end of the edge target id (<cluster>/<namespace>/<claim>).
func claimEdgesByName(body cytoscape.Body) map[string]*cytoscape.EdgeMetricsDTO {
	out := map[string]*cytoscape.EdgeMetricsDTO{}
	for _, e := range tierEdges(body, graph.StorageTierSVMPVC) {
		target := e.Data.Target
		for i := len(target) - 1; i >= 0; i-- {
			if target[i] == '/' {
				out[target[i+1:]] = e.Data.Metrics
				break
			}
		}
	}
	return out
}

func (s *ioDeltaSuite) fetchIODelta(base, aggr, ioDelta string, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.storageGraphURL(base, func(q url.Values) {
		q.Set("aggr", aggr)
		if ioDelta != "" {
			q.Set("io_delta", ioDelta)
		}
		if configure != nil {
			configure(q)
		}
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode, "io_delta=%q", ioDelta)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}
