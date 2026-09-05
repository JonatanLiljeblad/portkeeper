package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const MCPServerReady = "Ready"

// IsReady reports whether the current desired generation is ready for traffic.
func (m *MCPServer) IsReady() bool {
	if m == nil || !m.DeletionTimestamp.IsZero() || m.Status.ObservedGeneration != m.Generation {
		return false
	}
	condition := meta.FindStatusCondition(m.Status.Conditions, MCPServerReady)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == m.Generation
}
