package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGraph_ClusterRawName(t *testing.T) {
	t.Parallel()

	withTable := &Graph{ClusterIdentities: map[string]ClusterIdentity{
		"us-dev-c1":  {AZ: "us", Env: "dev", Name: "c1"},
		"eu-prod-c1": {AZ: "eu", Env: "prod", Name: "c1"},
	}}

	tests := []struct {
		name string
		g    *Graph
		id   string
		want string
	}{
		{"nil graph", nil, "us-dev-c1", "us-dev-c1"},
		{"nil table degrades to identity", &Graph{}, "cluster-alpha", "cluster-alpha"},
		{"present identity yields raw name", withTable, "us-dev-c1", "c1"},
		{"second identity of same raw name", withTable, "eu-prod-c1", "c1"},
		{"absent identity is verbatim", withTable, "cluster-beta", "cluster-beta"},
		{"a raw name is not an identity", withTable, "c1", "c1"},
		{"empty id", withTable, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.g.ClusterRawName(tc.id))
		})
	}
}
