package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// StampLabels is a pure function, so it needs no container — these run even
// when Docker is unavailable.
func TestStampLabels(t *testing.T) {
	t.Parallel()

	const extra = `az="zone-a",env="prod",cluster="k8s-1"`

	tests := map[string]struct {
		in    string
		extra string
		want  string
	}{
		"empty extra is a no-op": {
			in:    `kube_pod_info{cluster="a"} 1 100`,
			extra: "",
			want:  `kube_pod_info{cluster="a"} 1 100`,
		},
		"stamps every missing key": {
			in:    `kube_pod_info{pod="p"} 1 100`,
			extra: extra,
			want:  `kube_pod_info{pod="p",az="zone-a",env="prod",cluster="k8s-1"} 1 100`,
		},
		"spelled-out key wins": {
			in:    `kube_pod_info{pod="p",az="zone-b"} 1 100`,
			extra: extra,
			want:  `kube_pod_info{pod="p",az="zone-b",env="prod",cluster="k8s-1"} 1 100`,
		},
		// The regression this guards: `cluster=` is a SUFFIX of
		// `ontap_cluster=`, so a substring test would treat the Harvest series
		// as already carrying the Kubernetes cluster and never stamp it.
		"suffix collision does not suppress the stamp": {
			in:    `volume_labels{ontap_cluster="ontap-1",aggr="a1"} 1 100`,
			extra: extra,
			want:  `volume_labels{ontap_cluster="ontap-1",aggr="a1",az="zone-a",env="prod",cluster="k8s-1"} 1 100`,
		},
		"empty label set gets no leading comma": {
			in:    `up{} 1 100`,
			extra: `env="prod"`,
			want:  `up{env="prod"} 1 100`,
		},
		"braceless line gets a label set": {
			in:    `up 1 100`,
			extra: `env="prod"`,
			want:  `up{env="prod"} 1 100`,
		},
		"comments and blank lines are untouched": {
			in:    "# HELP up nothing\n\nup 1 100",
			extra: `env="prod"`,
			want:  "# HELP up nothing\n\n" + `up{env="prod"} 1 100`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, StampLabels(tc.in, tc.extra))
		})
	}
}

func TestHasLabelKey(t *testing.T) {
	t.Parallel()

	assert.True(t, hasLabelKey(`cluster="a"`, "cluster"), "match at the start of the set")
	assert.True(t, hasLabelKey(`pod="p",cluster="a"`, "cluster"), "match after a separator")
	assert.True(t, hasLabelKey(`pod="p", cluster="a"`, "cluster"), "match after a spaced separator")
	assert.False(t, hasLabelKey(`ontap_cluster="a"`, "cluster"), "suffix of another label name is not a match")
	assert.False(t, hasLabelKey(`pod="p"`, "cluster"), "absent key")
	assert.True(t, hasLabelKey(`ontap_cluster="a",cluster="b"`, "cluster"), "suffix collision then a real match")
}
