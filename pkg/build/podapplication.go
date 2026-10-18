package build

import (
	"cmp"
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
	// AppMetadata is the application-metadata source, nil to skip it. Empty
	// label fields read their defaults, and the value is validated: an
	// invalid one fails the call before any query.
	AppMetadata *promql.AppMetadataSource
}

// ErrAmbiguousPod reports that the raw (cluster, namespace, pod) names pods in
// more than one zone or environment, so no single Application applies.
var ErrAmbiguousPod = errors.New("pod application lookup: pod matches more than one zone or environment; set AZ or Env")

// ResolvePodApplication resolves one pod's ArgoCD Application on demand with
// the same rules the topology build applies to every pod. When req.AppMetadata
// is set and q serves the appmeta family, the lexically-smallest non-empty
// app on the pod's metric rows wins and the controller chain is not read.
// Otherwise the chain is unchanged: controller owner (ReplicaSet collapsed to
// its Deployment), that controller's tracking-id annotation, and the Job →
// CronJob hop when the Job carries none of its own. It returns "" and a nil
// error when the pod resolves no Application. Any upstream error fails the
// lookup — unlike the build, nothing degrades, because a skipped leg could
// substitute the CronJob's Application for a Job's.
//
// The pod-owner query runs first. Its rows pin az / env for every later query,
// including the metric query. When that query returns no rows and the metric
// step is active, the metric query is issued with the request's own az / env
// (no matcher for a dimension the request omitted) and the same pinning rule
// is applied to its rows. A querier that does not implement
// promql.FamilyServer, or one that does not serve appmeta, skips the metric
// step — the opposite of a graph build, which treats a plain querier as served.
func ResolvePodApplication(ctx context.Context, q LabelQuerier, req PodApplicationRequest) (string, error) {
	if req.AppMetadata != nil {
		src := req.AppMetadata.OrDefault()
		req.AppMetadata = &src
	}
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
	useMetric := lookupUsesAppMetadata(q, req.AppMetadata)
	if len(owners) == 0 {
		if !useMetric {
			return "", nil
		}
		rows, err := l.appMetadataRows(ctx)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "", nil
		}
		if err := l.pinScope(rows); err != nil {
			return "", err
		}
		app, _ := minLabel(rows, req.AppMetadata.App)
		return app, nil
	}
	if err := l.pinScope(owners); err != nil {
		return "", err
	}
	if useMetric {
		rows, err := l.appMetadataRows(ctx)
		if err != nil {
			return "", err
		}
		if app, ok := minLabel(rows, req.AppMetadata.App); ok {
			return app, nil
		}
	}

	kind, name, ok, err := l.controllerOwner(ctx, owners)
	if err != nil || !ok {
		return "", err
	}
	return resolveOwnerApplication(ctx, l, kind, name)
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
	if r.AppMetadata != nil {
		if err := r.AppMetadata.Validate(r.LabelKeys); err != nil {
			return fmt.Errorf("pod application lookup: app metadata: %w", err)
		}
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

// pinScope fixes every unset identity label to the single value the rows
// carry, so later queries cannot read a same-named object in another zone or
// environment. An absent label reads as "". The rows are the pod-owner series,
// or the application-metadata series when the pod has no controller owner.
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
	l.applyScope(&req)
	maps.Copy(req.Filters, filters)
	sets, err := l.q.QueryLabels(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("pod application lookup: %s: %w", metric, err)
	}
	return sets, nil
}

// lookupUsesAppMetadata reports whether this call reads the metric. A nil
// source skips it. So does a querier that does not implement FamilyServer, and
// a querier whose table serves appmeta on no backend. Do not share
// appMetadataServed: that helper treats a querier without FamilyServer as
// served, which is the graph build's default and the wrong one here.
func lookupUsesAppMetadata(q LabelQuerier, src *promql.AppMetadataSource) bool {
	if src == nil {
		return false
	}
	fs, ok := q.(promql.FamilyServer)
	return ok && fs.Serves(promql.FamilyAppMeta)
}

// appMetadataRows issues the one appmeta label query for this pod. The
// namespace and pod filters use the configured label names. az / env follow
// the pins already applied, so an ownerless call made before pinScope carries
// no matcher for a dimension the request omitted.
func (l *podAppLookup) appMetadataRows(ctx context.Context) ([]map[string]string, error) {
	src := l.req.AppMetadata
	req := promql.LabelQuery{
		Metric: src.Metric,
		Family: promql.FamilyAppMeta,
		Filters: map[string]string{
			"cluster":     l.req.Cluster,
			src.Namespace: l.req.Namespace,
			src.Pod:       l.req.Pod,
		},
		At:        l.req.At,
		Window:    l.req.Window,
		LabelKeys: l.req.LabelKeys,
	}
	l.applyScope(&req)
	sets, err := l.q.QueryLabels(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("pod application lookup: app_metadata: %w", err)
	}
	return sets, nil
}

// applyScope copies the pinned zone and environment onto req. A zone value
// routes and renders its matcher; a zone pinned to "absent" cannot route, so
// it is matched as a filter instead.
func (l *podAppLookup) applyScope(req *promql.LabelQuery) {
	switch {
	case l.az.set && l.az.val != "":
		req.AZ = l.az.val
	case l.az.set:
		req.Filters[l.keys.AZ] = ""
	}
	if l.env.set {
		req.Filters[l.keys.Env] = l.env.val
	}
}

// controllerOwner picks the pod's controller owner exactly as resolvePodOwners
// does: every ReplicaSet owner is collapsed to its Deployment FIRST, then the
// lexically-smallest (kind, name) wins. Rows missing either label are skipped.
// A pod normally has one controller row, so this issues at most one
// kube_replicaset_owner query.
func (l *podAppLookup) controllerOwner(ctx context.Context, owners []map[string]string) (kind, name string, ok bool, err error) {
	refs := make([]ownerRef, 0, len(owners))
	for _, s := range owners {
		if k, n := s["owner_kind"], s["owner_name"]; k != "" && n != "" {
			refs = append(refs, ownerRef{k, n})
		}
	}
	slices.SortFunc(refs, func(a, b ownerRef) int { return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.name, b.name)) })
	for _, ref := range slices.Compact(refs) {
		if ref.kind == "ReplicaSet" {
			rsOwners, err := l.query(ctx, promql.QReplicaSetOwner, map[string]string{
				"replicaset": ref.name,
				"owner_kind": "Deployment",
			})
			if err != nil {
				return "", "", false, err
			}
			if dep, found := minLabel(rsOwners, "owner_name"); found {
				ref = ownerRef{"Deployment", dep}
			}
		}
		if !ok || ownerLess(ref.kind, ref.name, kind, name) {
			kind, name, ok = ref.kind, ref.name, true
		}
	}
	return kind, name, ok, nil
}

// controllerApp reads one controller's tracking-id from its annotation family.
// ok is false for a kind with no family and for a controller carrying no
// usable tracking-id.
func (l *podAppLookup) controllerApp(ctx context.Context, kind, name string) (string, bool, error) {
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
		if raw := s[argoTrackingIDLabel]; betterTrackingID(best, raw) {
			best = raw
		}
	}
	if best == "" {
		return "", false, nil
	}
	return argoAppName(best), true, nil
}

// cronJobOf reads the CronJob controlling a Job from kube_job_owner.
func (l *podAppLookup) cronJobOf(ctx context.Context, job string) (string, bool, error) {
	jobOwners, err := l.query(ctx, promql.QJobOwner, map[string]string{
		"job_name":            job,
		"owner_kind":          "CronJob",
		"owner_is_controller": "true",
	})
	if err != nil {
		return "", false, err
	}
	cronJob, ok := minLabel(jobOwners, "owner_name")
	return cronJob, ok, nil
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
