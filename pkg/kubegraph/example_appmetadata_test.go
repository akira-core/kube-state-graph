package kubegraph_test

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// An embedder enables the application-metadata metric by filling
// Options.AppMetadata directly; the KSG_APP_METADATA_* environment variables
// are read by the server binary only. Empty label fields take their defaults.
func ExampleOptions_appMetadata() {
	keys := promql.LabelKeys{AZ: "az", Env: "env"}
	src := &promql.AppMetadataSource{
		Metric: "my_app_metadata",
		// Only the label names that differ from the defaults need setting.
		App:     "application",
		NSOwner: "team",
		NSAlias: "ns_display",
	}

	// Validate is the embedder's call; the library trusts a directly built
	// value, the way it trusts LabelKeys.
	if err := src.Validate(keys); err != nil {
		fmt.Println("invalid:", err)
		return
	}
	r := src.OrDefault()
	fmt.Println(r.Metric, r.App, r.User, r.NSOwner, r.Namespace, r.Pod)
	fmt.Println(r.DeployTime, r.NSGroup, r.NSGroupOwner, r.NSAlias)

	// A nil AppMetadata keeps the feature off: no query, unchanged body.
	eng := kubegraph.New(promqlfake.New(appMetadataFixtures()), kubegraph.Options{
		APITimeout:  10 * time.Second,
		LabelKeys:   keys,
		AppMetadata: src,
	})
	body, err := eng.BuildK8sFromValues(context.Background(), url.Values{
		"start":   {"1700000000"},
		"end":     {"1700003600"},
		"cluster": {"c1"},
		"az":      {"zone-a"},
		"env":     {"prod"},
		"pod":     {"shop/web-0"},
	})
	if err != nil {
		fmt.Println("build:", err)
		return
	}

	// The metric's values land on the synthesised groups, never in labels.
	for _, n := range body.Elements.Nodes {
		d := n.Data
		switch d.Type {
		case "namespace":
			fmt.Println(d.ID, d.NamespaceOwner, d.NamespaceGroup, d.NamespaceGroupOwner, d.NamespaceAlias)
		case "application":
			fmt.Println(d.ID, d.DeployedBy, d.DeployedAt)
		}
	}

	// Output:
	// my_app_metadata application user team namespace pod
	// deploy_time namespace_group namespace_group_owner ns_display
	// zone-a-prod-c1/namespace/shop team-shop commerce bob Shop
	// zone-a-prod-c1/namespace/shop/application/checkout alice 2025-10-08T13:14:15Z
}

// appMetadataFixtures is shop/web-0 on worker-1 plus one application-metadata
// row for it, spelled with the example's label bindings. Its deploy time is in
// the zone-less slash form, which is read as UTC.
func appMetadataFixtures() map[promql.Query]model.Vector {
	row := func(pairs ...string) *model.Sample {
		m := model.Metric{"cluster": "c1", "az": "zone-a", "env": "prod"}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
		}
		return &model.Sample{Metric: m, Value: 1}
	}
	return map[promql.Query]model.Vector{
		promql.QPodInfo:  {row("namespace", "shop", "pod", "web-0", "uid", "uid-web", "node", "worker-1")},
		promql.QNodeInfo: {row("node", "worker-1")},
		promql.QAppMetadata: {row(
			"namespace", "shop", "pod", "web-0",
			"application", "checkout", "user", "alice", "deploy_time", "2025/10/08 13:14:15",
			"team", "team-shop", "namespace_group", "commerce", "namespace_group_owner", "bob",
			"ns_display", "Shop",
		)},
	}
}

// Validate rejects a label that collides with the cluster label, the az / env
// keys, or another field — including a new field's default label.
func ExampleOptions_appMetadataInvalid() {
	keys := promql.LabelKeys{AZ: "az", Env: "env"}

	for _, src := range []promql.AppMetadataSource{
		{Metric: "my_app_metadata", User: "cluster"},
		{Metric: "my_app_metadata", App: "az"},
		{Metric: "my_app_metadata", App: "owner"},
		{Metric: "my_app_metadata", User: "namespace_alias"},
		{Metric: "my_app_metadata", User: "namespace_alias", NSAlias: "ns_display"},
		{Metric: "not a metric"},
	} {
		fmt.Println(src.Validate(keys))
	}

	// Output:
	// field "user" uses label "cluster", which collides with cluster or the configured az/env label
	// field "app" uses label "az", which collides with cluster or the configured az/env label
	// label "owner" is used by both "app" and "ns_owner"
	// label "namespace_alias" is used by both "user" and "ns_alias"
	// <nil>
	// metric "not a metric" is not a valid Prometheus metric name
}
