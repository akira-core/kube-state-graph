package kubegraph_test

import (
	"fmt"
	"time"

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
	}

	// Validate is the embedder's call; the library trusts a directly built
	// value, the way it trusts LabelKeys.
	if err := src.Validate(keys); err != nil {
		fmt.Println("invalid:", err)
		return
	}
	resolved := src.OrDefault()
	fmt.Println(resolved.Metric, resolved.App, resolved.User, resolved.NSOwner, resolved.Namespace, resolved.Pod)

	client, err := promql.New("http://localhost:8428", nil)
	if err != nil {
		fmt.Println("client:", err)
		return
	}
	defer client.CloseIdleConnections()

	// A nil AppMetadata keeps the feature off: no query, unchanged body.
	eng := kubegraph.New(client, kubegraph.Options{
		APITimeout:  10 * time.Second,
		LabelKeys:   keys,
		AppMetadata: src,
	})
	fmt.Println(eng != nil)

	// Output:
	// my_app_metadata application user team namespace pod
	// true
}

// Validate rejects a label that collides with the cluster label, the az / env
// keys, or another field.
func ExampleOptions_appMetadataInvalid() {
	keys := promql.LabelKeys{AZ: "az", Env: "env"}

	for _, src := range []promql.AppMetadataSource{
		{Metric: "my_app_metadata", User: "cluster"},
		{Metric: "my_app_metadata", App: "az"},
		{Metric: "my_app_metadata", App: "owner"},
		{Metric: "not a metric"},
	} {
		fmt.Println(src.Validate(keys))
	}

	// Output:
	// field "user" uses label "cluster", which collides with cluster or the configured az/env label
	// field "app" uses label "az", which collides with cluster or the configured az/env label
	// label "owner" is used by both "app" and "ns_owner"
	// metric "not a metric" is not a valid Prometheus metric name
}
