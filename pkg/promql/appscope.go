package promql

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TrackingIDLabel is kube-state-metrics' sanitised form of the
// argocd.argoproj.io/tracking-id annotation. The application recovery
// restricts the six controller-annotation families on it.
const TrackingIDLabel = "annotation_argocd_argoproj_io_tracking_id"

// TrackingIDWrapperCost is the rendered length of the fixed `(?:` … `)(?::.*)?`
// wrapper a tracking-id chunk is charged once. Callers subtract it from the
// byte budget before chunking, so the budget bounds the matcher that is sent
// and not only the alternation inside the group.
const TrackingIDWrapperCost = len("(?:") + len(")(?::.*)?")

func trackingIDFamily(q Query) bool {
	switch q {
	case QDeploymentAnnotations, QStatefulSetAnnotations, QDaemonSetAnnotations,
		QReplicaSetAnnotations, QJobAnnotations, QCronJobAnnotations:
		return true
	default:
		return false
	}
}

// RenderTrackingIDScoped renders one controller-annotation family restricted
// to tracking-ids whose segment before the first ":" is one of apps:
//
//	last_over_time(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!="",az="zone-a",annotation_argocd_argoproj_io_tracking_id=~"(?:checkout)(?::.*)?"}[5m])
//
// The fixed `!=""` selector and the request matchers are composed, never
// replaced. Each value is QuoteMeta-escaped; the optional `:` suffix is what
// lets a verbatim Application (no colon in the raw tracking-id) match too.
// PromQL anchors `=~`, so a value matches itself and nothing else.
//
// ok is false when apps holds no non-empty value or q is not one of the six
// annotation families. The caller MUST then skip the query.
func RenderTrackingIDScoped(q Query, window time.Duration, keys LabelKeys, sel Selector, apps []string) (string, bool) {
	if !trackingIDFamily(q) {
		return "", false
	}
	vals := normaliseValues(apps)
	if len(vals) == 0 {
		return "", false
	}
	var matchers []string
	if fixed := fixedSelector[q]; fixed != "" {
		matchers = append(matchers, fixed)
	}
	if req := sel.render(queryDims[q], keys); req != "" {
		matchers = append(matchers, req)
	}
	alts := make([]string, len(vals))
	for i, v := range vals {
		alts[i] = escapeLiteral(regexp.QuoteMeta(v))
	}
	matchers = append(matchers, TrackingIDLabel+`=~"(?:`+strings.Join(alts, "|")+`)(?::.*)?"`)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// RenderOwnerScoped renders an owner family restricted to owner names of one
// kind, composed with the family's fixed selector and the request matchers:
//
//	kube_replicaset_owner → owner_kind="<kind>",owner_name=~"…"
//	kube_pod_owner        → owner_is_controller="true",owner_kind="<kind>",owner_name=~"…"
//	kube_job_owner        → owner_name=~"…" only, and only when kind is CronJob
//	  (its fixed selector already pins owner_kind and owner_is_controller)
//
// ok is false for any other query, an empty kind, or an empty name set.
func RenderOwnerScoped(q Query, window time.Duration, keys LabelKeys, sel Selector, kind string, names []string) (string, bool) {
	if kind == "" {
		return "", false
	}
	vals := normaliseValues(names)
	if len(vals) == 0 {
		return "", false
	}
	var kindMatchers []string
	switch q {
	case QReplicaSetOwner:
		kindMatchers = []string{`owner_kind="` + escapeLiteral(kind) + `"`}
	case QPodOwner:
		kindMatchers = []string{`owner_is_controller="true"`, `owner_kind="` + escapeLiteral(kind) + `"`}
	case QJobOwner:
		if kind != "CronJob" {
			return "", false
		}
	default:
		return "", false
	}
	var matchers []string
	if fixed := fixedSelector[q]; fixed != "" {
		matchers = append(matchers, fixed)
	}
	if req := sel.render(queryDims[q], keys); req != "" {
		matchers = append(matchers, req)
	}
	matchers = append(matchers, kindMatchers...)
	matchers = appendMatcher(matchers, "owner_name", vals)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}
