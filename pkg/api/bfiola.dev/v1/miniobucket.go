// +k8s:deepcopy-gen=package

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioBucket defines a MinIO tenant bucket.
type MinioBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MinioBucketSpec   `json:"spec"`
	Status MinioBucketStatus `json:"status,omitempty"`
}

// MinioBucketSpec defines the desired state of MinioBucket
type MinioBucketSpec struct {
	Name      string      `json:"name"`
	TenantRef ResourceRef `json:"tenantRef"`
}

// MinioBucketStatus defines the current state of MinioBucket
type MinioBucketStatus struct {
	Synced      *bool            `json:"synced,omitempty"`
	CurrentSpec *MinioBucketSpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioBucketList contains a list of MinioBucket
type MinioBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioBucket `json:"items"`
}
