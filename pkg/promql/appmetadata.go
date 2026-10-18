package promql

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Default label names of the five application-metadata fields. A field left
// empty on AppMetadataSource reads its default (OrDefault).
const (
	DefaultAppMetadataAppLabel       = "argocd_app"
	DefaultAppMetadataUserLabel      = "user"
	DefaultAppMetadataNSOwnerLabel   = "owner"
	DefaultAppMetadataNamespaceLabel = "namespace"
	DefaultAppMetadataPodLabel       = "pod"
)

// AppMetadataSource is the operator's application-metadata metric: the series
// name plus the five label names its rows are read through. A nil pointer at
// the build options is the off switch; this value is only constructed once a
// metric name is configured. An empty label field means its default; the
// build and the lookup apply OrDefault before reading the value.
//
// Cluster is not a field: every row is read from the label "cluster".
// Availability zone and environment use the process-wide LabelKeys, the same
// keys kube-state-metrics series use.
//
// Label names are rendered into PromQL verbatim. internal/config runs
// Validate at startup; an embedder constructing the value directly is trusted
// code, as with LabelKeys, and should call Validate itself.
type AppMetadataSource struct {
	Metric    string
	App       string
	User      string
	NSOwner   string
	Namespace string
	Pod       string
}

// OrDefault returns a copy with every empty label field set to its default.
// Metric is never defaulted.
func (s AppMetadataSource) OrDefault() AppMetadataSource {
	s.App = cmp.Or(s.App, DefaultAppMetadataAppLabel)
	s.User = cmp.Or(s.User, DefaultAppMetadataUserLabel)
	s.NSOwner = cmp.Or(s.NSOwner, DefaultAppMetadataNSOwnerLabel)
	s.Namespace = cmp.Or(s.Namespace, DefaultAppMetadataNamespaceLabel)
	s.Pod = cmp.Or(s.Pod, DefaultAppMetadataPodLabel)
	return s
}

// Validate checks the value after OrDefault: Metric is a legal metric name,
// every label a legal label name, no two fields share a label, and no field
// uses "cluster" or the az / env key of keys. Each error names the field by
// its configuration name (app, user, ns_owner, namespace, pod).
func (s AppMetadataSource) Validate(keys LabelKeys) error {
	if !metricNameRe.MatchString(s.Metric) {
		return fmt.Errorf("metric %q is not a valid Prometheus metric name", s.Metric)
	}
	keys = keys.OrDefault()
	s = s.OrDefault()
	used := make(map[string]string, 5)
	for _, f := range []struct{ field, label string }{
		{"app", s.App}, {"user", s.User}, {"ns_owner", s.NSOwner},
		{"namespace", s.Namespace}, {"pod", s.Pod},
	} {
		if !labelNameRe.MatchString(f.label) {
			return fmt.Errorf("label for field %q is not a valid PromQL label name: %q", f.field, f.label)
		}
		if f.label == "cluster" || f.label == keys.AZ || f.label == keys.Env {
			return fmt.Errorf("field %q uses label %q, which collides with cluster or the configured az/env label", f.field, f.label)
		}
		if other, ok := used[f.label]; ok {
			return fmt.Errorf("label %q is used by both %q and %q", f.label, other, f.field)
		}
		used[f.label] = f.field
	}
	return nil
}

// RenderAppMetadata renders the unscoped application-metadata leg:
//
//	tlast_over_time(<metric>{…}[w])
//
// The metric name is src.Metric, never the logical query name app_metadata.
// Request matchers follow queryDims for QAppMetadata (az, env, cluster,
// namespace), with the namespace matcher on src.Namespace. A zero selector
// renders no braces.
func RenderAppMetadata(src AppMetadataSource, window time.Duration, keys LabelKeys, sel Selector) string {
	return renderAppMetadata(src, window, keys, sel, "", nil)
}

// RenderAppMetadataByNamespace renders the metric restricted to one cluster
// and its namespaces: the request matchers first, then the cluster
// restriction, then the namespace alternation. cluster is a bucketed
// kube-state-metrics cluster name, so ClusterUnknownValue renders the
// unknown-or-absent form. The namespace restriction uses src.Namespace, so a
// request that also carries ?namespace= produces two matchers on that label,
// and a request ?cluster= likewise composes with the cluster restriction.
//
// ok is false when cluster is empty, namespaces holds no non-empty value, or
// the namespace label is unset. The caller MUST skip the query: an empty
// scope is not a licence to read the metric unscoped.
func RenderAppMetadataByNamespace(src AppMetadataSource, window time.Duration, keys LabelKeys, sel Selector, cluster string, namespaces []string) (string, bool) {
	if src.Namespace == "" || cluster == "" || len(normaliseValues(namespaces)) == 0 {
		return "", false
	}
	matchers := appMetadataClusterMatchers(src, keys, sel, cluster)
	return formatAppMetadata(src, window, appendMatcher(matchers, src.Namespace, namespaces)), true
}

// RenderAppMetadataByApp renders the metric restricted to Application names
// on src.App. ok is false when apps holds no non-empty value or the app label
// is unset. The caller MUST skip the query rather than fall back to an
// unscoped read.
func RenderAppMetadataByApp(src AppMetadataSource, window time.Duration, keys LabelKeys, sel Selector, apps []string) (string, bool) {
	if src.App == "" || len(normaliseValues(apps)) == 0 {
		return "", false
	}
	return renderAppMetadata(src, window, keys, sel, src.App, apps), true
}

// AppMetadataMatcherCost is the rendered byte length of the request matchers
// RenderAppMetadataByApp repeats in every chunk of a restricted read. It is
// the length of that fragment: the joining comma a chunker splices is not
// added, and a zero selector costs nothing.
func AppMetadataMatcherCost(src AppMetadataSource, keys LabelKeys, sel Selector) int {
	return len(strings.Join(appMetadataRequestMatchers(src, keys, sel), ","))
}

// AppMetadataClusterMatcherCost is AppMetadataMatcherCost for
// RenderAppMetadataByNamespace: the request matchers plus the cluster
// restriction every chunk of that cluster repeats. It is never zero.
func AppMetadataClusterMatcherCost(src AppMetadataSource, keys LabelKeys, sel Selector, cluster string) int {
	return len(strings.Join(appMetadataClusterMatchers(src, keys, sel, cluster), ","))
}

func renderAppMetadata(src AppMetadataSource, window time.Duration, keys LabelKeys, sel Selector, restrictKey string, restrict []string) string {
	matchers := appMetadataRequestMatchers(src, keys, sel)
	if restrictKey != "" {
		matchers = appendMatcher(matchers, restrictKey, restrict)
	}
	return formatAppMetadata(src, window, matchers)
}

func formatAppMetadata(src AppMetadataSource, window time.Duration, matchers []string) string {
	body := ""
	if len(matchers) > 0 {
		body = "{" + strings.Join(matchers, ",") + "}"
	}
	return fmt.Sprintf("tlast_over_time(%s%s[%s])", src.Metric, body, FormatDuration(window))
}

// appMetadataClusterMatchers is the request matchers followed by the cluster
// restriction of a namespace-restricted read.
func appMetadataClusterMatchers(src AppMetadataSource, keys LabelKeys, sel Selector, cluster string) []string {
	return appendClusterMatcher(appMetadataRequestMatchers(src, keys, sel), []string{cluster})
}

// appMetadataRequestMatchers renders az, env, cluster, then namespace, in
// that order. The namespace key is the configured label, not the literal
// "namespace". Cluster keeps the unknown-bucket form appendClusterMatcher
// already uses for every other dimCluster query.
func appMetadataRequestMatchers(src AppMetadataSource, keys LabelKeys, sel Selector) []string {
	keys = keys.OrDefault()
	var out []string
	out = appendMatcher(out, keys.AZ, sel.AZ)
	out = appendMatcher(out, keys.Env, sel.Env)
	out = appendClusterMatcher(out, sel.Cluster)
	if src.Namespace != "" {
		out = appendMatcher(out, src.Namespace, sel.Namespace)
	}
	return out
}
