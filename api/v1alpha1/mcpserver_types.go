package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCPServerSpec defines the desired state of an MCPServer.
type MCPServerSpec struct {
	// Image is the container image running the MCP server.
	Image string `json:"image"`

	// Port is the port the MCP server listens on inside the container.
	Port int32 `json:"port"`

	// Tools lists the tool names this server exposes. Used by the gateway
	// to route requests without having to query the server itself first.
	// +optional
	Tools []string `json:"tools,omitempty"`

	// AuthType describes how the gateway should authenticate to this
	// server. v0.1 only supports "none" and "token".
	// +kubebuilder:validation:Enum=none;token
	// +kubebuilder:default=none
	AuthType string `json:"authType,omitempty"`

	// AuthSecretRef optionally names a Secret containing credentials,
	// required when AuthType is "token".
	// +optional
	AuthSecretRef string `json:"authSecretRef,omitempty"`
}

// MCPServerStatus defines the observed state of an MCPServer.
type MCPServerStatus struct {
	// Phase is a coarse human-readable status: Pending, Ready, Failed.
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the generation most recently reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes condition pattern.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcps
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// MCPServer is the Schema for the mcpservers API. Declaring one of these
// tells the controller to run an MCP server and tells the gateway it
// exists and is routable.
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec,omitempty"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer.
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
