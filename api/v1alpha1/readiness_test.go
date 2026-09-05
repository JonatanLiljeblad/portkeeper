package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsReady(t *testing.T) {
	ready := &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: MCPServerStatus{ObservedGeneration: 3, Conditions: []metav1.Condition{{
			Type: MCPServerReady, Status: metav1.ConditionTrue, ObservedGeneration: 3,
		}}},
	}
	tests := []struct {
		name   string
		mutate func(*MCPServer)
		want   bool
	}{
		{"current", func(m *MCPServer) {}, true},
		{"stale status", func(m *MCPServer) { m.Status.ObservedGeneration = 2 }, false},
		{"stale condition", func(m *MCPServer) { m.Status.Conditions[0].ObservedGeneration = 2 }, false},
		{"missing condition", func(m *MCPServer) { m.Status.Conditions = nil }, false},
		{"wrong condition", func(m *MCPServer) { m.Status.Conditions[0].Type = "Other" }, false},
		{"false", func(m *MCPServer) { m.Status.Conditions[0].Status = metav1.ConditionFalse }, false},
		{"unknown", func(m *MCPServer) { m.Status.Conditions[0].Status = metav1.ConditionUnknown }, false},
		{"deleting", func(m *MCPServer) { now := metav1.Now(); m.DeletionTimestamp = &now }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ready.DeepCopy()
			tt.mutate(m)
			if got := m.IsReady(); got != tt.want {
				t.Fatalf("IsReady() = %v, want %v", got, tt.want)
			}
		})
	}
	if (*MCPServer)(nil).IsReady() {
		t.Fatal("nil MCPServer must not be ready")
	}
}
