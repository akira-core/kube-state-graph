package kubegraph_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
)

// An embedder gets the exact /v1/k8s-graph body in-process: the Kubernetes
// neighbourhood of one root, from the same parameters the route takes. A
// request carrying no root is a *ParseError before any upstream query.
func ExampleEngine_BuildK8sFromValues() {
	eng := kubegraph.New(promqlfake.New(k8sFixtures()), kubegraph.Options{APITimeout: 10 * time.Second})

	params := url.Values{
		"start":   {"1700000000"},
		"end":     {"1700003600"},
		"cluster": {"c1"},
		"az":      {"zone-a"},
		"env":     {"prod"},
		"node":    {"worker-1"},
	}
	body, err := eng.BuildK8sFromValues(context.Background(), params)
	fmt.Println(err, body.Clusters, len(body.Elements.Nodes) > 0)

	params.Del("node")
	_, err = eng.BuildK8sFromValues(context.Background(), params)
	var pe *kubegraph.ParseError
	if errors.As(err, &pe) {
		fmt.Println(pe.Reason)
	}

	// Output:
	// <nil> [zone-a-prod-c1] true
	// missing_root
}
