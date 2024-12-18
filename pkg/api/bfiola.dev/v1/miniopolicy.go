package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,description="Tenant Namespace",JSONPath=`.status.currentSpec.tenantRef.namespace`
// +kubebuilder:printcolumn:name="Tenant Name",type=string,description="Tenant Name",JSONPath=`.status.currentSpec.tenantRef.name`
// +kubebuilder:printcolumn:name="Name",type=string,description="Name",JSONPath=`.status.currentSpec.name`

// MinioPolicy defines a MinIO tenant policy
type MinioPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioPolicySpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioPolicyStatus `json:"status,omitempty"`
}

// MinioPolicyStatement defines a single statement within a MinioPolicy
type MinioPolicyStatement struct {
	Action   []string `json:"action"`
	Effect   string   `json:"effect"`
	Resource []string `json:"resource"`
}

// MinioPolicySpec defines the desired state of MinioPolicy
type MinioPolicySpec struct {
	Migrate   bool                   `json:"migrate,omitempty"`
	Name      string                 `json:"name"`
	Statement []MinioPolicyStatement `json:"statement"`
	TenantRef ResourceRef            `json:"tenantRef"`
	Version   string                 `json:"version"`
}

// MinioPolicyStatus defines the current state of MinioPolicy
type MinioPolicyStatus struct {
	Synced      *bool            `json:"synced,omitempty"`
	CurrentSpec *MinioPolicySpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioPolicyList contains a list of MinioPolicy
type MinioPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioPolicy `json:"items"`
}
