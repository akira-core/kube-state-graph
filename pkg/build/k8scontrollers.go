package build

import (
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// controllerReads decides which controller-owner and controller-annotation reads
// a Kubernetes-only build issues in each round, and remembers every name it has
// already asked a family for, so a name that more than one round's pods share —
// a StatefulSet owning a root pod and a co-mounter — is read once.
//
// The eight families split into two stages (controllerScope / deploymentScope /
// cronJobScope): the first reads what the loaded pods' owners name directly; the
// second reads the Deployments and CronJobs only the first stage's
// ReplicaSet-owner and Job-owner rows reveal.
//
// The second stage is read AHEAD of the first (design D6). The Deployment and
// CronJob controllers name what they create after themselves — a ReplicaSet
// `<deployment>-<pod-template-hash>`, a Job `<cronjob>-<scheduled-time>` — so
// the names the first stage is about to resolve can be guessed from the pods'
// owner rows, and the guess is read in the first stage's own round. Once the
// first stage returns, secondStage reads whatever it resolved that the guess did
// not cover, so a wrong or missing guess costs a round and never a row. A row the
// guess returned that no loaded pod's owner chain consults leaves the body
// unchanged: the annotation indexes are read only by that chain.
type controllerReads struct {
	// requested is, per family, every name a read has already been issued for.
	requested map[promql.Query]map[string]struct{}
	// directDeployments and directCronJobs are the Deployment and CronJob names
	// kube_pod_owner names directly, accumulated over every round's pods.
	directDeployments, directCronJobs []string
}

// unrequested returns the names of names no read of q has been issued for yet,
// sorted and de-duplicated, and records them as requested.
func (c *controllerReads) unrequested(q promql.Query, names []string) []string {
	if c.requested == nil {
		c.requested = map[promql.Query]map[string]struct{}{}
	}
	seen := c.requested[q]
	if seen == nil {
		seen = map[string]struct{}{}
		c.requested[q] = seen
	}
	var out []string
	for _, n := range sortedNames(names) {
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// firstStage returns the reads of the controllers the pods' kube_pod_owner rows
// name: the ReplicaSet and Job owner and annotation families, and the
// StatefulSet and DaemonSet annotations; and, ahead of time, the Deployment and
// CronJob annotations of the names those owners imply. Each is restricted to the
// names no earlier round has read.
func (c *controllerReads) firstStage(v *topologyVectors, podOwner model.Vector) []nameRead {
	byKind := controllerScope(podOwner)
	c.directDeployments = append(c.directDeployments, byKind["Deployment"]...)
	c.directCronJobs = append(c.directCronJobs, byKind["CronJob"]...)
	deployments := slices.Concat(byKind["Deployment"], withoutFinalSegment(byKind["ReplicaSet"]))
	cronJobs := slices.Concat(byKind["CronJob"], withoutFinalSegment(byKind["Job"]))
	return c.nonEmpty([]nameRead{
		{query: promql.QReplicaSetOwner, slot: &v.ReplicaSetOwner, scope: c.unrequested(promql.QReplicaSetOwner, byKind["ReplicaSet"])},
		{query: promql.QReplicaSetAnnotations, slot: &v.ReplicaSetAnnotations, scope: c.unrequested(promql.QReplicaSetAnnotations, byKind["ReplicaSet"])},
		{query: promql.QJobOwner, slot: &v.JobOwner, scope: c.unrequested(promql.QJobOwner, byKind["Job"])},
		{query: promql.QJobAnnotations, slot: &v.JobAnnotations, scope: c.unrequested(promql.QJobAnnotations, byKind["Job"])},
		{query: promql.QStatefulSetAnnotations, slot: &v.StatefulSetAnnotations, scope: c.unrequested(promql.QStatefulSetAnnotations, byKind["StatefulSet"])},
		{query: promql.QDaemonSetAnnotations, slot: &v.DaemonSetAnnotations, scope: c.unrequested(promql.QDaemonSetAnnotations, byKind["DaemonSet"])},
		{query: promql.QDeploymentAnnotations, slot: &v.DeploymentAnnotations, scope: c.unrequested(promql.QDeploymentAnnotations, deployments)},
		{query: promql.QCronJobAnnotations, slot: &v.CronJobAnnotations, scope: c.unrequested(promql.QCronJobAnnotations, cronJobs)},
	})
}

// withoutFinalSegment is each name with its final `-` segment removed — the
// owner a ReplicaSet or Job name implies. A name with no segment to remove
// implies nothing, and secondStage picks its owner up once it is known.
func withoutFinalSegment(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if i := strings.LastIndexByte(n, '-'); i > 0 {
			out = append(out, n[:i])
		}
	}
	return out
}

// secondStage returns the reads of the Deployments and CronJobs the loaded
// ReplicaSet-owner and Job-owner rows resolve to (deploymentScope,
// cronJobScope), restricted to the names no earlier read of the family covered.
// It reads the accumulated owner vectors, so it must run after the rounds that
// landed them.
func (c *controllerReads) secondStage(v *topologyVectors) []nameRead {
	return c.nonEmpty([]nameRead{
		{
			query: promql.QDeploymentAnnotations, slot: &v.DeploymentAnnotations,
			scope: c.unrequested(promql.QDeploymentAnnotations, deploymentScope(c.directDeployments, v.ReplicaSetOwner)),
		},
		{
			query: promql.QCronJobAnnotations, slot: &v.CronJobAnnotations,
			scope: c.unrequested(promql.QCronJobAnnotations, cronJobScope(c.directCronJobs, v.JobOwner)),
		},
	})
}

// nonEmpty drops the reads with nothing to restrict on, so a round that has no
// controller work issues, and counts, nothing.
func (c *controllerReads) nonEmpty(reads []nameRead) []nameRead {
	return slices.DeleteFunc(reads, func(r nameRead) bool { return len(r.scope) == 0 })
}
