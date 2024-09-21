package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioPolicyBinding attaches a MinIO identity to a MinIO policy
type MinioPolicyBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MinioPolicyBindingSpec   `json:"spec"`
	Status MinioPolicyBindingStatus `json:"status,omitempty"`
}

// MinioPolicyBindingSpec defines the desired state of MinioPolicyBinding
type MinioPolicyBindingSpec struct {
	TenantRef ResourceRef `json:"tenantRef"`
}

// MinioPolicyBindingStatus defines the desired state of MinioPolicyBinding
type MinioPolicyBindingStatus struct {
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioPolicyBindingList contains a list of MinioPolicyBinding
type MinioPolicyBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioPolicyBinding `json:"items"`
}
