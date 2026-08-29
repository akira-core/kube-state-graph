package backendsfile_test

import (
	"context"
	"fmt"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
)

// The embedding path documented in docs/upstream-backend-routing.md: the
// operator's routing file, the router over it, the reload loop, and the engine
// that dispatches through it. Compiled here so a signature change fails the
// build instead of leaving the documentation quietly wrong.
func Example() {
	ctx := context.Background()

	// A nil lookup reads the process environment for the credential variables
	// the file names.
	table, err := backendsfile.Read("/etc/ksg/backends.yaml", nil)
	if err != nil {
		// An invalid file at startup is fatal: there is no previously-good
		// table to fall back to.
		fmt.Println("routing table:", err)
		return
	}

	router, err := promql.NewRouter(table, nil, nil) // nil metrics, default client factory
	if err != nil {
		fmt.Println("router:", err)
		return
	}

	// Optional logger and metrics: nil means silent and unrecorded.
	backendsfile.Start(ctx, router, backendsfile.ReloaderOptions{
		Path: "/etc/ksg/backends.yaml",
	}, 30*time.Second)

	engine := kubegraph.NewRouted(router, kubegraph.Options{APITimeout: 30 * time.Second})

	end := time.Now().UTC()
	if _, err := engine.Build(ctx, 5*time.Minute, end, promql.Selector{AZ: []string{"zone-a"}}); err != nil {
		fmt.Println("build:", err)
	}
	// Output:
	// routing table: backends file /etc/ksg/backends.yaml: open /etc/ksg/backends.yaml: no such file or directory
}

// A single upstream endpoint needs no file at all.
func ExampleParse() {
	table, err := backendsfile.Parse([]byte(`
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-a]
  - name: netapp
    url: http://vm-netapp:8428
    families: [harvest]
`), func(string) (string, bool) { return "", false })
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(table.Len(), "backends")

	single, err := promql.SingleBackendTable("http://vm.example:8428", "", "")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(single.Backends()[0].Name())
	// Output:
	// 2 backends
	// default
}
