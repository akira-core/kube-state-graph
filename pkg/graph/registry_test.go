package graph

import (
	"strings"
	"testing"
)

func TestEdgeTypePodRoutesToService_Registered(t *testing.T) {
	var def *EdgeTypeDefinition
	for i := range EdgeTypes {
		if EdgeTypes[i].Type == EdgeTypePodRoutesToService {
			def = &EdgeTypes[i]
			break
		}
	}
	if def == nil {
		t.Fatal("EdgeTypePodRoutesToService is not registered in EdgeTypes")
	}
	if !def.Directed {
		t.Error("pod-routes-to-service must be directed")
	}
	if def.MayCrossCluster {
		t.Error("pod-routes-to-service is intra-cluster by construction (locked ingress cluster) — may_cross_cluster must be false")
	}
	if len(def.SourceType) != 1 || def.SourceType[0] != NodeTypePod {
		t.Errorf("pod-routes-to-service source_type = %v, want exactly [pod]", def.SourceType)
	}
	if len(def.TargetType) != 1 || def.TargetType[0] != NodeTypeService {
		t.Errorf("pod-routes-to-service target_type = %v, want exactly [service]", def.TargetType)
	}
	if !strings.Contains(def.Description, "NOT observed traffic") {
		t.Errorf("pod-routes-to-service description must state the edge is config-derived, not observed traffic; got %q", def.Description)
	}
	if !ValidEdgeType(EdgeTypePodRoutesToService) {
		t.Error("ValidEdgeType must accept pod-routes-to-service so ?edge_type=pod-routes-to-service parses")
	}
}
