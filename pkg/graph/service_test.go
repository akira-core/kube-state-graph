package graph

import "testing"

func TestServiceNode_Contract(t *testing.T) {
	s := &ServiceNode{
		IDValue:        ServiceID("cluster-alpha", "payments-ns", "payments"),
		NameValue:      "payments",
		LabelsValue:    map[string]string{"cluster": "cluster-alpha", "namespace": "payments-ns"},
		IPAddressValue: []string{"10.0.0.5"},
	}

	if got, want := s.ID(), "cluster-alpha/payments-ns/payments"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	if got, want := s.Name(), "payments"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := s.Type(), NodeTypeService; got != want {
		t.Errorf("Type() = %q, want %q", got, want)
	}
	if got := s.Labels()["cluster"]; got != "cluster-alpha" {
		t.Errorf("Labels()[cluster] = %q, want cluster-alpha", got)
	}
	if got := s.IPAddress(); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("IPAddress() = %v, want [10.0.0.5]", got)
	}

	// Sealed-interface participation.
	var _ GraphNode = s
}

func TestServiceNode_HeadlessOmitsIP(t *testing.T) {
	// A headless service (cluster_ip="None") carries no IP — the resolver
	// passes a nil/empty IPAddressValue, and IPAddress() must return nil.
	s := &ServiceNode{
		IDValue:     ServiceID("c", "ns", "headless"),
		NameValue:   "headless",
		LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"},
	}
	if got := s.IPAddress(); got != nil {
		t.Errorf("IPAddress() = %v, want nil for headless service", got)
	}
}

func TestServiceID_Format(t *testing.T) {
	if got, want := ServiceID("c", "ns", "svc"), "c/ns/svc"; got != want {
		t.Errorf("ServiceID = %q, want %q", got, want)
	}
}

func TestEdgeTypeServiceSelectsPod_Registered(t *testing.T) {
	var serviceSelectsPod *EdgeTypeDefinition
	var podCallsPod *EdgeTypeDefinition
	var podCallsService *EdgeTypeDefinition
	for i := range EdgeTypes {
		switch EdgeTypes[i].Type {
		case EdgeTypeServiceSelectsPod:
			serviceSelectsPod = &EdgeTypes[i]
		case EdgeTypePodCallsPod:
			podCallsPod = &EdgeTypes[i]
		case EdgeTypePodCallsService:
			podCallsService = &EdgeTypes[i]
		default:
			// other edge types are not under test here
		}
	}

	if serviceSelectsPod == nil {
		t.Fatal("EdgeTypeServiceSelectsPod is not registered in EdgeTypes")
	}
	if !serviceSelectsPod.MayCrossCluster {
		t.Error("service-selects-pod may cross clusters via the same-family endpoint union (may_cross_cluster=true)")
	}
	if !containsNodeType(serviceSelectsPod.SourceType, NodeTypeService) {
		t.Errorf("service-selects-pod source_type = %v, want to contain service", serviceSelectsPod.SourceType)
	}
	if !containsNodeType(serviceSelectsPod.TargetType, NodeTypePod) {
		t.Errorf("service-selects-pod target_type = %v, want to contain pod", serviceSelectsPod.TargetType)
	}

	if podCallsPod == nil {
		t.Fatal("EdgeTypePodCallsPod is not registered")
	}
	if containsNodeType(podCallsPod.TargetType, NodeTypeService) {
		t.Errorf("pod-calls-pod target_type = %v, must NOT include service (service targets use pod-calls-service)", podCallsPod.TargetType)
	}
	if podCallsService == nil {
		t.Fatal("EdgeTypePodCallsService is not registered")
	}
	if !podCallsService.MayCrossCluster {
		t.Error("pod-calls-service may cross clusters: a route-engine-resolved endpoint anchors on the selected ingress cluster, which may be a family sibling of the caller's (may_cross_cluster=true)")
	}
	if !containsNodeType(podCallsService.TargetType, NodeTypeService) {
		t.Errorf("pod-calls-service target_type = %v, want to contain service", podCallsService.TargetType)
	}
	if len(podCallsService.TargetType) != 1 {
		t.Errorf("pod-calls-service target_type = %v, want exactly [service]", podCallsService.TargetType)
	}
}

func containsNodeType(types []NodeType, want NodeType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}
