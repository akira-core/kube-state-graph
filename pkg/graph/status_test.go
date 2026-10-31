package graph

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFoldStatus(t *testing.T) {
	tests := []struct {
		name        string
		alerts      []Alert
		health      string
		readyStatus string
		want        string
	}{
		{name: "no signals", want: StatusNormal},
		{name: "warning alert", alerts: []Alert{{Severity: "warning"}}, want: StatusWarning},
		{name: "critical alert", alerts: []Alert{{Severity: "critical"}}, want: StatusCritical},
		{name: "severity is case insensitive", alerts: []Alert{{Severity: "CrItIcAl"}}, want: StatusCritical},
		{name: "missing severity warns", alerts: []Alert{{}}, want: StatusWarning},
		{name: "unrecognised severity warns", alerts: []Alert{{Severity: "page"}}, want: StatusWarning},
		{name: "informational severities do not tint", alerts: []Alert{{Severity: "info"}, {Severity: "none"}}, want: StatusNormal},
		{name: "degraded health is critical", health: HealthDegraded, want: StatusCritical},
		{name: "online health has no effect", health: HealthOnline, want: StatusNormal},
		{name: "NotReady is critical", readyStatus: ReadyStatusNotReady, want: StatusCritical},
		{name: "Unknown is warning", readyStatus: ReadyStatusUnknown, want: StatusWarning},
		{name: "Ready has no effect", readyStatus: ReadyStatusReady, want: StatusNormal},
		{
			name:        "worst signal wins",
			alerts:      []Alert{{Severity: "warning"}, {Severity: "critical"}},
			health:      HealthOnline,
			readyStatus: ReadyStatusUnknown,
			want:        StatusCritical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FoldStatus(tt.alerts, tt.health, tt.readyStatus))
		})
	}
}

func TestFoldStatus_AlertOrderDoesNotMatter(t *testing.T) {
	alerts := []Alert{{Severity: "none"}, {Severity: "warning"}, {Severity: "critical"}, {Severity: "page"}}
	want := FoldStatus(alerts, "", "")

	for seed := range 20 {
		shuffled := append([]Alert(nil), alerts...)
		rand.New(rand.NewSource(int64(seed))).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		assert.Equal(t, want, FoldStatus(shuffled, "", ""))
	}
}

// An alert's last-seen time is not a status input: severity alone folds. A
// critical alert last seen long before the window's end, and one whose time did
// not resolve, both still fold to critical.
func TestFoldStatus_IgnoresAlertTime(t *testing.T) {
	old := Alert{Name: "A", State: AlertStateFiring, Severity: "critical", Time: time.Unix(1, 0)}
	unresolved := Alert{Name: "A", State: AlertStateFiring, Severity: "critical"}
	assert.Equal(t, StatusCritical, FoldStatus([]Alert{old}, "", ""))
	assert.Equal(t, StatusCritical, FoldStatus([]Alert{unresolved}, "", ""))
	assert.Equal(t,
		FoldStatus([]Alert{{Severity: "warning", Time: time.Unix(1761897720, 0)}}, "", ""),
		FoldStatus([]Alert{{Severity: "warning"}}, "", ""))
}

func TestGraphNodeStatusAccessors(t *testing.T) {
	carrying := []GraphNode{
		&PodNode{StatusValue: StatusNormal},
		&K8sNode{StatusValue: StatusWarning},
		&PVCNode{StatusValue: StatusCritical},
		&NetAppNode{StatusValue: StatusNormal},
		&NetAppAggrNode{StatusValue: StatusWarning},
	}
	assert.Equal(t,
		[]string{StatusNormal, StatusWarning, StatusCritical, StatusNormal, StatusWarning},
		[]string{carrying[0].Status(), carrying[1].Status(), carrying[2].Status(), carrying[3].Status(), carrying[4].Status()})

	for _, n := range []GraphNode{&ServiceNode{}, &ExternalNode{}, &NetAppSVMNode{}} {
		assert.Empty(t, n.Status())
	}
}
