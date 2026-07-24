package graph

import (
	"testing"
)

func TestEdgeTypePodCallsService_MayCrossCluster(t *testing.T) {
	var def *EdgeTypeDefinition
	for i := range EdgeTypes {
		if EdgeTypes[i].Type == EdgeTypePodCallsService {
			def = &EdgeTypes[i]
			break
		}
	}
	if def == nil {
		t.Fatal("EdgeTypePodCallsService is not registered in EdgeTypes")
	}
	if !def.MayCrossCluster {
		t.Error("pod-calls-service may_cross_cluster must be true (route-engine ingress-cluster anchoring)")
	}
	if !ValidEdgeType(EdgeTypePodCallsService) {
		t.Error("ValidEdgeType must accept pod-calls-service")
	}
}
