package build

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

var podAppAt = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// labelStore answers QueryLabels from in-memory series the way the upstream
// would: every filter and the az matcher is an equality, an absent label
// reading as "". It records each request for assertions.
type labelStore struct {
	series map[string][]map[string]string
	fail   string
	reqs   []promql.LabelQuery
}

func (s *labelStore) add(metric string, labels map[string]string) {
	if s.series == nil {
		s.series = map[string][]map[string]string{}
	}
	s.series[metric] = append(s.series[metric], labels)
}

// QueryLabels makes labelStore the LabelQuerier. pkg/build/mocks imports
// pkg/build, so an in-package test cannot use the generated mock.
func (s *labelStore) QueryLabels(_ context.Context, req promql.LabelQuery) ([]map[string]string, error) {
	s.reqs = append(s.reqs, req)
	if req.Metric == s.fail {
		return nil, errors.New("boom")
	}
	var out []map[string]string
	for _, labels := range s.series[req.Metric] {
		if req.AZ != "" && labels[req.LabelKeys.OrDefault().AZ] != req.AZ {
			continue
		}
		match := true
		for k, v := range req.Filters {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, labels)
		}
	}
	return out, nil
}

func (s *labelStore) metrics() []string {
	out := make([]string, len(s.reqs))
	for i, r := range s.reqs {
		out[i] = r.Metric
	}
	return out
}

// scoped stamps the identity labels every fixture series carries.
func scoped(env, cluster string, labels map[string]string) map[string]string {
	out := map[string]string{"az": "zone-a", "env": env, "cluster": cluster, "namespace": "shop"}
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func podAppRequest(pod string) PodApplicationRequest {
	return PodApplicationRequest{
		AZ: "zone-a", Cluster: "c1", Namespace: "shop", Pod: pod,
		At: podAppAt, Window: 5 * time.Minute,
	}
}

func podOwner(env, cluster, pod, kind, name string) map[string]string {
	return scoped(env, cluster, map[string]string{
		"pod": pod, "owner_kind": kind, "owner_name": name, "owner_is_controller": "true",
	})
}

func annotation(env, cluster, nameLabel, name, trackingID string) map[string]string {
	return scoped(env, cluster, map[string]string{nameLabel: name, argoTrackingIDLabel: trackingID})
}

// podAppFixture covers every resolution path the batch resolver has.
func podAppFixture() *labelStore {
	s := &labelStore{}
	// Deployment via ReplicaSet collapse.
	s.add("kube_pod_owner", podOwner("prod", "c1", "web-abc", "ReplicaSet", "web-7d9"))
	s.add("kube_replicaset_owner", scoped("prod", "c1", map[string]string{"replicaset": "web-7d9", "owner_kind": "Deployment", "owner_name": "web"}))
	s.add("kube_deployment_annotations", annotation("prod", "c1", "deployment", "web", "shop-app:apps/Deployment:shop/web"))
	// Bare ReplicaSet with its own annotation.
	s.add("kube_pod_owner", podOwner("prod", "c1", "bare-xyz", "ReplicaSet", "bare-rs"))
	s.add("kube_replicaset_annotations", annotation("prod", "c1", "replicaset", "bare-rs", "bare-app:apps/ReplicaSet:shop/bare-rs"))
	// StatefulSet with a malformed sibling that must not win the min-pick.
	s.add("kube_pod_owner", podOwner("prod", "c1", "db-0", "StatefulSet", "db"))
	s.add("kube_statefulset_annotations", annotation("prod", "c1", "statefulset", "db", ":apps/StatefulSet:shop/db"))
	s.add("kube_statefulset_annotations", annotation("prod", "c1", "statefulset", "db", "zz-db:apps/StatefulSet:shop/db"))
	s.add("kube_statefulset_annotations", annotation("prod", "c1", "statefulset", "db", "db-app:apps/StatefulSet:shop/db"))
	// Job with its own annotation: the CronJob hop must not run.
	s.add("kube_pod_owner", podOwner("prod", "c1", "own-job-1", "Job", "own-job"))
	s.add("kube_job_annotations", annotation("prod", "c1", "job_name", "own-job", "job-app"))
	s.add("kube_job_owner", scoped("prod", "c1", map[string]string{"job_name": "own-job", "owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true"}))
	// Job without an annotation: resolves through its CronJob.
	s.add("kube_pod_owner", podOwner("prod", "c1", "cron-job-1", "Job", "cron-job"))
	s.add("kube_job_owner", scoped("prod", "c1", map[string]string{"job_name": "cron-job", "owner_kind": "CronJob", "owner_name": "nightly", "owner_is_controller": "true"}))
	s.add("kube_cronjob_annotations", annotation("prod", "c1", "cronjob", "nightly", "batch-app:batch/CronJob:shop/nightly"))
	// Owner kind with no annotation family.
	s.add("kube_pod_owner", podOwner("prod", "c1", "rc-1", "ReplicationController", "rc"))
	// Two controller owners: the lexically-smallest (kind, name) wins.
	s.add("kube_pod_owner", podOwner("prod", "c1", "multi-1", "StatefulSet", "db"))
	s.add("kube_pod_owner", podOwner("prod", "c1", "multi-1", "DaemonSet", "agent"))
	s.add("kube_daemonset_annotations", annotation("prod", "c1", "daemonset", "agent", "agent-app"))
	// Same raw names in another cluster must never leak in.
	s.add("kube_pod_owner", podOwner("prod", "c2", "web-abc", "StatefulSet", "db"))
	s.add("kube_deployment_annotations", annotation("prod", "c2", "deployment", "web", "other-cluster-app"))
	// Non-controller owner row is ignored.
	s.add("kube_pod_owner", scoped("prod", "c1", map[string]string{"pod": "orphan", "owner_kind": "Deployment", "owner_name": "web", "owner_is_controller": "false"}))
	return s
}

func TestResolvePodApplication(t *testing.T) {
	tests := []struct {
		pod     string
		want    string
		queries []string
	}{
		{"web-abc", "shop-app", []string{"kube_pod_owner", "kube_replicaset_owner", "kube_deployment_annotations"}},
		{"bare-xyz", "bare-app", []string{"kube_pod_owner", "kube_replicaset_owner", "kube_replicaset_annotations"}},
		{"db-0", "db-app", []string{"kube_pod_owner", "kube_statefulset_annotations"}},
		{"own-job-1", "job-app", []string{"kube_pod_owner", "kube_job_annotations"}},
		{"cron-job-1", "batch-app", []string{"kube_pod_owner", "kube_job_annotations", "kube_job_owner", "kube_cronjob_annotations"}},
		{"rc-1", "", []string{"kube_pod_owner"}},
		{"multi-1", "agent-app", []string{"kube_pod_owner", "kube_daemonset_annotations"}},
		{"orphan", "", []string{"kube_pod_owner"}},
		{"missing", "", []string{"kube_pod_owner"}},
	}
	for _, tc := range tests {
		t.Run(tc.pod, func(t *testing.T) {
			s := podAppFixture()
			got, err := ResolvePodApplication(t.Context(), s, podAppRequest(tc.pod))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.queries, s.metrics())
		})
	}
}

func TestResolvePodApplication_RequestShape(t *testing.T) {
	s := podAppFixture()
	_, err := ResolvePodApplication(t.Context(), s, podAppRequest("web-abc"))
	require.NoError(t, err)
	require.Len(t, s.reqs, 3)

	first := s.reqs[0]
	assert.Equal(t, promql.FamilyKSM, first.Family)
	assert.Equal(t, "zone-a", first.AZ)
	assert.Equal(t, podAppAt, first.At)
	assert.Equal(t, 5*time.Minute, first.Window)
	assert.Equal(t, map[string]string{
		"cluster": "c1", "namespace": "shop", "pod": "web-abc", "owner_is_controller": "true",
	}, first.Filters, "env is not filtered before it is known")

	// Later legs carry the environment pinned from the pod-owner series.
	assert.Equal(t, map[string]string{
		"cluster": "c1", "namespace": "shop", "env": "prod", "replicaset": "web-7d9", "owner_kind": "Deployment",
	}, s.reqs[1].Filters)
	assert.Equal(t, map[string]string{
		"cluster": "c1", "namespace": "shop", "env": "prod", "deployment": "web",
	}, s.reqs[2].Filters)
	for _, r := range s.reqs {
		assert.Equal(t, promql.FamilyKSM, r.Family)
	}
}

func TestResolvePodApplication_Environment(t *testing.T) {
	newStore := func() *labelStore {
		s := &labelStore{}
		s.add("kube_pod_owner", podOwner("prod", "c1", "api-1", "Deployment", "api"))
		s.add("kube_pod_owner", podOwner("staging", "c1", "api-1", "Deployment", "api"))
		s.add("kube_deployment_annotations", annotation("prod", "c1", "deployment", "api", "prod-app"))
		s.add("kube_deployment_annotations", annotation("staging", "c1", "deployment", "api", "staging-app"))
		return s
	}

	t.Run("ambiguous without env", func(t *testing.T) {
		s := newStore()
		_, err := ResolvePodApplication(t.Context(), s, podAppRequest("api-1"))
		require.ErrorIs(t, err, ErrAmbiguousPod)
		assert.Equal(t, []string{"kube_pod_owner"}, s.metrics())
	})

	t.Run("env pins every leg", func(t *testing.T) {
		s := newStore()
		req := podAppRequest("api-1")
		req.Env = "staging"
		got, err := ResolvePodApplication(t.Context(), s, req)
		require.NoError(t, err)
		assert.Equal(t, "staging-app", got)
		for _, r := range s.reqs {
			assert.Equal(t, "staging", r.Filters["env"])
		}
	})

	t.Run("configured env key", func(t *testing.T) {
		s := &labelStore{}
		s.add("kube_pod_owner", map[string]string{"zone": "zone-a", "stage": "prod", "cluster": "c1", "namespace": "shop",
			"pod": "api-1", "owner_kind": "Deployment", "owner_name": "api", "owner_is_controller": "true"})
		s.add("kube_deployment_annotations", map[string]string{"zone": "zone-a", "stage": "prod", "cluster": "c1", "namespace": "shop",
			"deployment": "api", argoTrackingIDLabel: "api-app"})
		req := podAppRequest("api-1")
		req.LabelKeys = promql.LabelKeys{AZ: "zone", Env: "stage"}
		got, err := ResolvePodApplication(t.Context(), s, req)
		require.NoError(t, err)
		assert.Equal(t, "api-app", got)
		assert.Equal(t, "prod", s.reqs[1].Filters["stage"])
	})
}

func TestResolvePodApplication_UpstreamErrorFails(t *testing.T) {
	for _, metric := range []string{"kube_pod_owner", "kube_job_annotations", "kube_job_owner", "kube_cronjob_annotations"} {
		t.Run(metric, func(t *testing.T) {
			s := podAppFixture()
			s.fail = metric
			got, err := ResolvePodApplication(t.Context(), s, podAppRequest("cron-job-1"))
			require.ErrorContains(t, err, metric)
			assert.Empty(t, got)
		})
	}
}

func TestResolvePodApplication_Validation(t *testing.T) {
	tests := map[string]func(*PodApplicationRequest){
		"cluster":   func(r *PodApplicationRequest) { r.Cluster = "" },
		"namespace": func(r *PodApplicationRequest) { r.Namespace = "" },
		"pod":       func(r *PodApplicationRequest) { r.Pod = "" },
		"at":        func(r *PodApplicationRequest) { r.At = time.Time{} },
		"window":    func(r *PodApplicationRequest) { r.Window = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			req := podAppRequest("web-abc")
			mutate(&req)
			s := &labelStore{}
			_, err := ResolvePodApplication(t.Context(), s, req)
			require.Error(t, err)
			assert.Empty(t, s.reqs, "validation fails before any upstream call")
		})
	}
}

// TestResolvePodApplication_MatchesBatchResolver feeds one fixture to the
// topology build's resolver chain and to the on-demand lookup, and requires
// the same Application for every pod.
func TestResolvePodApplication_MatchesBatchResolver(t *testing.T) {
	s := podAppFixture()
	vec := func(metric string) model.Vector {
		out := make(model.Vector, 0, len(s.series[metric]))
		for _, labels := range s.series[metric] {
			m := model.Metric{}
			for k, v := range labels {
				m[model.LabelName(k)] = model.LabelValue(v)
			}
			out = append(out, &model.Sample{Metric: m, Value: 1})
		}
		return out
	}
	v := topologyVectors{
		DeploymentAnnotations:  vec("kube_deployment_annotations"),
		StatefulSetAnnotations: vec("kube_statefulset_annotations"),
		DaemonSetAnnotations:   vec("kube_daemonset_annotations"),
		ReplicaSetAnnotations:  vec("kube_replicaset_annotations"),
		JobAnnotations:         vec("kube_job_annotations"),
		CronJobAnnotations:     vec("kube_cronjob_annotations"),
	}
	mc := newClusterResolver(promql.LabelKeys{})
	batch := resolvePodApplications(
		resolvePodOwners(vec("kube_pod_owner"), vec("kube_replicaset_owner"), mc),
		resolveControllerApplications(v, mc),
		resolveJobCronJobOwners(vec("kube_job_owner"), mc),
		false,
	)

	checked := 0
	for _, labels := range s.series["kube_pod_owner"] {
		if labels["cluster"] != "c1" {
			continue
		}
		pod := labels["pod"]
		m := model.Metric{"az": "zone-a", "env": "prod", "cluster": "c1"}
		want := batch[podNameKey{mc.bucket(promql.QPodOwner, m), "shop", pod}]

		for _, az := range []string{"zone-a", ""} {
			req := podAppRequest(pod)
			req.AZ = az
			got, err := ResolvePodApplication(t.Context(), &labelStore{series: s.series}, req)
			require.NoError(t, err)
			assert.Equal(t, want, got, "pod %s az %q", pod, az)
		}
		checked++
	}
	require.NotZero(t, checked)
}

// inZone moves a fixture series to another zone.
func inZone(az string, labels map[string]string) map[string]string {
	labels["az"] = az
	return labels
}

func TestResolvePodApplication_Zone(t *testing.T) {
	newStore := func() *labelStore {
		s := &labelStore{}
		s.add("kube_pod_owner", podOwner("prod", "c1", "api-1", "Deployment", "api"))
		s.add("kube_deployment_annotations", annotation("prod", "c1", "deployment", "api", "zone-a-app"))
		s.add("kube_deployment_annotations", inZone("zone-b", annotation("prod", "c1", "deployment", "api", "zone-b-app")))
		return s
	}

	t.Run("unset az is pinned from the pod owner", func(t *testing.T) {
		s := newStore()
		req := podAppRequest("api-1")
		req.AZ = ""
		got, err := ResolvePodApplication(t.Context(), s, req)
		require.NoError(t, err)
		assert.Equal(t, "zone-a-app", got, "the zone-b Deployment of the same name must not be read")

		require.Len(t, s.reqs, 2)
		assert.Empty(t, s.reqs[0].AZ, "the pod-owner query reaches every ksm backend")
		assert.NotContains(t, s.reqs[0].Filters, "az")
		assert.Equal(t, "zone-a", s.reqs[1].AZ, "later queries route by the pinned zone")
		assert.NotContains(t, s.reqs[1].Filters, "az")
	})

	t.Run("pod in two zones is ambiguous without az", func(t *testing.T) {
		s := newStore()
		s.add("kube_pod_owner", inZone("zone-b", podOwner("prod", "c1", "api-1", "Deployment", "api")))
		req := podAppRequest("api-1")
		req.AZ = ""
		_, err := ResolvePodApplication(t.Context(), s, req)
		require.ErrorIs(t, err, ErrAmbiguousPod)
		assert.Equal(t, []string{"kube_pod_owner"}, s.metrics())
	})

	t.Run("az resolves the collision", func(t *testing.T) {
		s := newStore()
		s.add("kube_pod_owner", inZone("zone-b", podOwner("prod", "c1", "api-1", "Deployment", "api")))
		req := podAppRequest("api-1")
		req.AZ = "zone-b"
		got, err := ResolvePodApplication(t.Context(), s, req)
		require.NoError(t, err)
		assert.Equal(t, "zone-b-app", got)
	})

	t.Run("absent zone label is pinned as a filter", func(t *testing.T) {
		s := &labelStore{}
		s.add("kube_pod_owner", map[string]string{"env": "prod", "cluster": "c1", "namespace": "shop",
			"pod": "api-1", "owner_kind": "Deployment", "owner_name": "api", "owner_is_controller": "true"})
		s.add("kube_deployment_annotations", map[string]string{"env": "prod", "cluster": "c1", "namespace": "shop",
			"deployment": "api", argoTrackingIDLabel: "unzoned-app"})
		s.add("kube_deployment_annotations", annotation("prod", "c1", "deployment", "api", "zoned-app"))
		req := podAppRequest("api-1")
		req.AZ = ""
		got, err := ResolvePodApplication(t.Context(), s, req)
		require.NoError(t, err)
		assert.Equal(t, "unzoned-app", got)
		require.Len(t, s.reqs, 2)
		assert.Empty(t, s.reqs[1].AZ)
		az, ok := s.reqs[1].Filters["az"]
		assert.True(t, ok, "absent zone is matched by an az=\"\" filter")
		assert.Empty(t, az)
	})
}
