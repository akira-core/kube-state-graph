package build

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// argoTrackingIDLabel is kube-state-metrics' sanitised form of the
// argocd.argoproj.io/tracking-id annotation.
const argoTrackingIDLabel = "annotation_argocd_argoproj_io_tracking_id"

// LabelQuerier is the upstream seam ResolvePodApplication reads through.
// *promql.Router satisfies it.
type LabelQuerier interface {
	QueryLabels(ctx context.Context, req promql.LabelQuery) ([]map[string]string, error)
}

// PodApplicationRequest names one pod by the (cluster, namespace, pod) tuple
// the topology build keys pods on. Cluster is the RAW cluster label, the same
// value `?cluster=` takes. AZ and Env are optional, as they are on the graph
// request: an empty one is pinned from the pod-owner series, and a pod found
// under more than one (az, env) fails with ErrAmbiguousPod. A set AZ routes
// every query to that zone's kube-state-metrics backends; an unset one lets
// the pod-owner query reach every ksm backend and routes the rest by the
// pinned zone.
type PodApplicationRequest struct {
	AZ        string
	Env       string
	Cluster   string
	Namespace string
	Pod       string
	At        time.Time
	Window    time.Duration
	LabelKeys promql.LabelKeys
}

// ErrAmbiguousPod reports that the raw (cluster, namespace, pod) names pods in
// more than one zone or environment, so no single Application applies.
var ErrAmbiguousPod = errors.New("pod application lookup: pod matches more than one zone or environment; set AZ or Env")

// ResolvePodApplication resolves one pod's ArgoCD Application on demand with
// the same rules the topology build applies to every pod: controller owner
// (ReplicaSet collapsed to its Deployment), that controller's tracking-id
// annotation, and the Job → CronJob hop when the Job carries none of its own.
// It returns "" and a nil error when the pod resolves no Application. Any
// upstream error fails the lookup — unlike the build, nothing degrades,
// because a skipped leg could substitute the CronJob's Application for a Job's.
func ResolvePodApplication(ctx context.Context, q LabelQuerier, req PodApplicationRequest) (string, error) {
	if err := req.validate(); err != nil {
		return "", err
	}
	l := &podAppLookup{
		q:    q,
		req:  req,
		keys: req.LabelKeys.OrDefault(),
		az:   pinOf(req.AZ),
		env:  pinOf(req.Env),
	}

	owners, err := l.query(ctx, promql.QPodOwner, map[string]string{
		"pod":                 req.Pod,
		"owner_is_controller": "true",
	})
	if err != nil {
		return "", err
	}
	if len(owners) == 0 {
		return "", nil
	}
	if err := l.pinScope(owners); err != nil {
		return "", err
	}

	kind, name, ok := minOwner(owners)
	if !ok {
		return "", nil
	}
	if kind == "ReplicaSet" {
		rsOwners, err := l.query(ctx, promql.QReplicaSetOwner, map[string]string{
			"replicaset": name,
			"owner_kind": "Deployment",
		})
		if err != nil {
			return "", err
		}
		if dep, ok := minLabel(rsOwners, "owner_name"); ok {
			kind, name = "Deployment", dep
		}
	}

	app, ok, err := l.controllerApplication(ctx, kind, name)
	if err != nil || ok || kind != "Job" {
		return app, err
	}

	jobOwners, err := l.query(ctx, promql.QJobOwner, map[string]string{
		"job_name":            name,
		"owner_kind":          "CronJob",
		"owner_is_controller": "true",
	})
	if err != nil {
		return "", err
	}
	cronJob, ok := minLabel(jobOwners, "owner_name")
	if !ok {
		return "", nil
	}
	app, _, err = l.controllerApplication(ctx, "CronJob", cronJob)
	return app, err
}

func (r PodApplicationRequest) validate() error {
	for _, f := range []struct{ name, val string }{
		{"Cluster", r.Cluster}, {"Namespace", r.Namespace}, {"Pod", r.Pod},
	} {
		if f.val == "" {
			return fmt.Errorf("pod application lookup: %s is required", f.name)
		}
	}
	if r.At.IsZero() {
		return errors.New("pod application lookup: evaluation instant is required")
	}
	if r.Window <= 0 {
		return errors.New("pod application lookup: window must be positive")
	}
	return nil
}

// pin is one identity label value the lookup scopes its queries by. An unset
// pin adds no matcher; a set pin with an empty value matches the label absent.
type pin struct {
	val string
	set bool
}

func pinOf(v string) pin { return pin{v, v != ""} }

type podAppLookup struct {
	q    LabelQuerier
	req  PodApplicationRequest
	keys promql.LabelKeys
	az   pin
	env  pin
}

// pinScope fixes every unset identity label to the single value the pod-owner
// rows carry, so later queries cannot read a same-named controller in another
// zone or environment. An absent label reads as "".
func (l *podAppLookup) pinScope(owners []map[string]string) error {
	first := owners[0]
	for _, s := range owners[1:] {
		if (!l.az.set && s[l.keys.AZ] != first[l.keys.AZ]) || (!l.env.set && s[l.keys.Env] != first[l.keys.Env]) {
			return ErrAmbiguousPod
		}
	}
	if !l.az.set {
		l.az = pin{first[l.keys.AZ], true}
	}
	if !l.env.set {
		l.env = pin{first[l.keys.Env], true}
	}
	return nil
}

func (l *podAppLookup) query(ctx context.Context, metric promql.Query, filters map[string]string) ([]map[string]string, error) {
	req := promql.LabelQuery{
		Metric:    string(metric),
		Family:    promql.FamilyKSM,
		Filters:   map[string]string{"cluster": l.req.Cluster, "namespace": l.req.Namespace},
		At:        l.req.At,
		Window:    l.req.Window,
		LabelKeys: l.req.LabelKeys,
	}
	// A zone value routes and renders its matcher; a zone pinned to "absent"
	// cannot route, so it is matched as a filter instead.
	switch {
	case l.az.set && l.az.val != "":
		req.AZ = l.az.val
	case l.az.set:
		req.Filters[l.keys.AZ] = ""
	}
	if l.env.set {
		req.Filters[l.keys.Env] = l.env.val
	}
	maps.Copy(req.Filters, filters)
	sets, err := l.q.QueryLabels(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("pod application lookup: %s: %w", metric, err)
	}
	return sets, nil
}

// controllerApplication reads one controller's tracking-id from its
// annotation family. ok is false for a kind with no family and for a
// controller carrying no usable tracking-id.
func (l *podAppLookup) controllerApplication(ctx context.Context, kind, name string) (string, bool, error) {
	i := slices.IndexFunc(controllerAnnotationFamilies, func(f controllerAnnotationFamily) bool { return f.kind == kind })
	if i < 0 {
		return "", false, nil
	}
	f := controllerAnnotationFamilies[i]
	sets, err := l.query(ctx, f.query, map[string]string{string(f.nameLabel): name})
	if err != nil {
		return "", false, err
	}
	best := ""
	for _, s := range sets {
		if raw := s[argoTrackingIDLabel]; usableTrackingID(raw) && (best == "" || raw < best) {
			best = raw
		}
	}
	if best == "" {
		return "", false, nil
	}
	return argoAppName(best), true, nil
}

// minOwner mirrors resolvePodOwners' tie-break: lexically-smallest
// (owner_kind, owner_name), skipping rows missing either.
func minOwner(sets []map[string]string) (kind, name string, ok bool) {
	for _, s := range sets {
		k, n := s["owner_kind"], s["owner_name"]
		if k == "" || n == "" {
			continue
		}
		if !ok || k < kind || (k == kind && n < name) {
			kind, name, ok = k, n, true
		}
	}
	return kind, name, ok
}

// minLabel returns the lexically-smallest non-empty value of label.
func minLabel(sets []map[string]string, label string) (string, bool) {
	best := ""
	for _, s := range sets {
		if v := s[label]; v != "" && (best == "" || v < best) {
			best = v
		}
	}
	return best, best != ""
}
