package build

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// controllerTargets are the eight kube-state-metrics owner /
// controller-annotation families a by-reference plan reads by reference, with
// the slot each lands in. They are exactly promql.ControllerScopedQueries.
func controllerTargets(v *topologyVectors) []scopedTarget {
	return []scopedTarget{
		{promql.QReplicaSetOwner, &v.ReplicaSetOwner},
		{promql.QReplicaSetAnnotations, &v.ReplicaSetAnnotations},
		{promql.QJobOwner, &v.JobOwner},
		{promql.QJobAnnotations, &v.JobAnnotations},
		{promql.QDeploymentAnnotations, &v.DeploymentAnnotations},
		{promql.QStatefulSetAnnotations, &v.StatefulSetAnnotations},
		{promql.QDaemonSetAnnotations, &v.DaemonSetAnnotations},
		{promql.QCronJobAnnotations, &v.CronJobAnnotations},
	}
}

// sortedNames returns names, deduplicated and sorted, with empty values
// dropped. It does not mutate its input.
func sortedNames(names []string) []string {
	out := slices.Clone(names)
	out = slices.DeleteFunc(out, func(n string) bool { return n == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// controllerScope groups the loaded pods' resolved CONTROLLER owner names by
// kind — exactly the rows resolvePodOwners itself consults (owner_is_controller
// == "true", non-empty owner_kind/owner_name) — mirroring the reader's own
// discard so a name enters the scope iff it could resolve an owner
// (scope-controller-legs-by-reference).
//
// This is the DIRECT owner kind kube_pod_owner names — "ReplicaSet" for a
// pod owned by a ReplicaSet regardless of whether that ReplicaSet itself
// resolves to a Deployment, and "Job" regardless of whether that Job itself
// resolves to a CronJob. The two-hop kinds (Deployment via ReplicaSet, CronJob
// via Job) are recovered separately by deploymentScope / cronJobScope, once
// the ReplicaSet / Job owner families have themselves been read.
func controllerScope(podOwner model.Vector) map[string][]string {
	byKind := map[string][]string{}
	for _, s := range podOwner {
		if string(s.Metric["owner_is_controller"]) != "true" {
			continue
		}
		kind := string(s.Metric["owner_kind"])
		name := string(s.Metric["owner_name"])
		if kind == "" || name == "" {
			continue
		}
		byKind[kind] = append(byKind[kind], name)
	}
	for k := range byKind {
		byKind[k] = sortedNames(byKind[k])
	}
	return byKind
}

// deploymentScope is the scope kube_deployment_annotations is read by
// reference against: the union of (a) pods DIRECTLY owned by a Deployment
// (direct — controllerScope's "Deployment" bucket) and (b) every Deployment
// name a loaded kube_replicaset_owner series resolves a ReplicaSet up to — the
// SAME filter resolvePodOwners applies (owner_kind == "Deployment"), so the
// scope this renders is exactly the set resolvePodOwners' rsToDeployment pass
// can ever consult.
func deploymentScope(direct []string, rsOwner model.Vector) []string {
	names := slices.Clone(direct)
	for _, s := range rsOwner {
		if string(s.Metric["owner_kind"]) != "Deployment" {
			continue
		}
		if n := string(s.Metric["owner_name"]); n != "" {
			names = append(names, n)
		}
	}
	return sortedNames(names)
}

// cronJobScope is the scope kube_cronjob_annotations is read by reference
// against: the union of (a) pods DIRECTLY owned by a CronJob (direct —
// controllerScope's "CronJob" bucket, a kind no stock controller ever
// produces but surfaced defensively) and (b) every owner_name a loaded
// kube_job_owner series carries. kube_job_owner is itself read at the fixed
// selector owner_kind="CronJob",owner_is_controller="true"
// (jobOwnerCronJobSelector), so every row it can return already names a
// CronJob controller — no further filtering is needed here.
func cronJobScope(direct []string, jobOwner model.Vector) []string {
	names := slices.Clone(direct)
	for _, s := range jobOwner {
		if n := string(s.Metric["owner_name"]); n != "" {
			names = append(names, n)
		}
	}
	return sortedNames(names)
}

// readScopedControllers issues the eight controller-owner /
// controller-annotation families restricted to the controller names the
// loaded pods' resolved owners name, in two stages
// (scope-controller-legs-by-reference).
//
// Stage A scopes six families directly off the loaded pods' owner refs
// (controllerScope): kube_replicaset_owner / kube_replicaset_annotations on
// the ReplicaSet names, kube_job_owner / kube_job_annotations on the Job
// names, kube_statefulset_annotations on the StatefulSet names,
// kube_daemonset_annotations on the DaemonSet names. Stage B, which waits for
// stage A's whole sub-wave to return, scopes the two families whose names are
// only known ONE HOP LATER: kube_deployment_annotations on
// deploymentScope(direct Deployment owners, the now-landed
// v.ReplicaSetOwner), and kube_cronjob_annotations on cronJobScope(direct
// CronJob owners, the now-landed v.JobOwner).
//
// A kind no loaded pod is owned by — and, for stage B, a two-hop kind no
// stage-A series resolved — issues no query for its families at all: the
// scope is empty, so issueScopedFamilies neither launches nor tallies it.
//
// It waits on the pod wave alone (podsDone), for the same happens-before
// reason readScopedNodes does.
func readScopedControllers(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	podsDone <-chan struct{},
) error {
	select {
	case <-podsDone:
	case <-ctx.Done():
		return nil
	}

	byKind := controllerScope(v.PodOwner)

	stageA := []scopedFamily{
		{query: promql.QReplicaSetOwner, dst: &v.ReplicaSetOwner, scope: byKind["ReplicaSet"]},
		{query: promql.QReplicaSetAnnotations, dst: &v.ReplicaSetAnnotations, scope: byKind["ReplicaSet"]},
		{query: promql.QJobOwner, dst: &v.JobOwner, scope: byKind["Job"]},
		{query: promql.QJobAnnotations, dst: &v.JobAnnotations, scope: byKind["Job"]},
		{query: promql.QStatefulSetAnnotations, dst: &v.StatefulSetAnnotations, scope: byKind["StatefulSet"]},
		{query: promql.QDaemonSetAnnotations, dst: &v.DaemonSetAnnotations, scope: byKind["DaemonSet"]},
	}
	if err := issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, stageA); err != nil {
		return err
	}

	stageB := []scopedFamily{
		{
			query: promql.QDeploymentAnnotations, dst: &v.DeploymentAnnotations,
			scope: deploymentScope(byKind["Deployment"], v.ReplicaSetOwner),
		},
		{
			query: promql.QCronJobAnnotations, dst: &v.CronJobAnnotations,
			scope: cronJobScope(byKind["CronJob"], v.JobOwner),
		},
	}
	return issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, stageB)
}
