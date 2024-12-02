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

// MinioGroup defines a MinIO builtin group identity.
type MinioGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioGroupSpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioGroupStatus `json:"status,omitempty"`
}

// MinioGroupSpec defines the desired state of MinioGroup
type MinioGroupSpec struct {
	Migrate   bool        `json:"migrate,omitempty"`
	Name      string      `json:"name"`
	TenantRef ResourceRef `json:"tenantRef"`
}

// MinioGroupStatus defines the current state of MinioGroup
type MinioGroupStatus struct {
	Synced      *bool           `json:"synced,omitempty"`
	CurrentSpec *MinioGroupSpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioGroupList contains a list of MinioGroup
type MinioGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioGroup `json:"items"`
}
