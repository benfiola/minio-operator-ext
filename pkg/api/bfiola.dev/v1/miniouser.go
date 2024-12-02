package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,description="Tenant Namespace",JSONPath=`.status.currentSpec.tenantRef.namespace`
// +kubebuilder:printcolumn:name="Tenant Name",type=string,description="Tenant Name",JSONPath=`.status.currentSpec.tenantRef.name`
// +kubebuilder:printcolumn:name="Access Key",type=string,description="Access Key",JSONPath=`.status.currentSpec.accessKey`

// MinioUser defines a MinIO builtin user identity.
type MinioUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioUserSpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioUserStatus `json:"status,omitempty"`
}

// MinioUserSpec defines the desired state of MinioUser
type MinioUserSpec struct {
	AccessKey    string         `json:"accessKey"`
	Migrate      bool           `json:"migrate,omitempty"`
	SecretKeyRef ResourceKeyRef `json:"secretKeyRef"`
	TenantRef    ResourceRef    `json:"tenantRef"`
}

// MinioUserStatus defines the current state of MinioUser
type MinioUserStatus struct {
	Synced                             *bool          `json:"synced,omitempty"`
	CurrentSpec                        *MinioUserSpec `json:"currentSpec,omitempty"`
	CurrentSecretKeyRefResourceVersion string         `json:"currentSecretKeyRefResourceVersion,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioUserList contains a list of MinioUser
type MinioUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioUser `json:"items"`
}
