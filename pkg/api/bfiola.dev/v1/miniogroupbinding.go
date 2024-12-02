package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,description="Tenant Namespace",JSONPath=`.status.currentSpec.tenantRef.namespace`
// +kubebuilder:printcolumn:name="Tenant Name",type=string,description="Tenant Name",JSONPath=`.status.currentSpec.tenantRef.name`
// +kubebuilder:printcolumn:name="User",type=string,description="User",JSONPath=`.status.currentSpec.user`
// +kubebuilder:printcolumn:name="Group",type=string,description="Group",JSONPath=`.status.currentSpec.group`

// MinioGroupBinding attaches a MinIO builtin user identity to a MinIO builtin group identity
type MinioGroupBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioGroupBindingSpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioGroupBindingStatus `json:"status,omitempty"`
}

// MinioGroupBindingSpec defines the desired state of MinioGroupBinding
type MinioGroupBindingSpec struct {
	Group     string      `json:"group"`
	Migrate   bool        `json:"migrate,omitempty"`
	TenantRef ResourceRef `json:"tenantRef,omitempty"`
	User      string      `json:"user"`
}

// MinioGroupBindingStatus defines the current state of MinioGroupBinding
type MinioGroupBindingStatus struct {
	Synced      *bool                  `json:"synced,omitempty"`
	CurrentSpec *MinioGroupBindingSpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioGroupBindingList contains a list of MinioGroupBinding
type MinioGroupBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioGroupBinding `json:"items"`
}
