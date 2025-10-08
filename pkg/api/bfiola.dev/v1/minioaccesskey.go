/*
Copyright (C) 2025  Ben Fiola

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
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

// MinioAccessKey defines a MinIO builtin service account identity.
// NOTE: in the minio API this is called a service account,
// but the documentation refers to it as an access key
// https://docs.min.io/community/minio-object-store/administration/identity-access-management/minio-user-management.html#access-keys
type MinioAccessKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioAccessKeySpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioAccessKeyStatus `json:"status,omitempty"`
}

// MinioAccessKeyIdentity represents an identity attached to a MinioAccessKey
type MinioAccessKeyIdentity struct {
	Builtin string `json:"builtin,omitempty"`
	Ldap    string `json:"ldap,omitempty"`
}

// MinioAccessKeyPolicy represents a policy attached to a MinioAccessKey
type MinioAccessKeyPolicy struct {
	Statement []MinioPolicyStatement `json:"statement"`
	Version   string                 `json:"version"`
}

// MinioAccessKeySpec defines the desired state of MinioAccessKey
type MinioAccessKeySpec struct {
	// populated by reconciler
	AccessKey   string       `json:"accessKey,omitempty"`
	Description string       `json:"description,omitempty"`
	Expiration  *metav1.Time `json:"expiration,omitempty"`
	Migrate     bool         `json:"migrate,omitempty"`
	// +kubebuilder:validation:Required
	Name   string               `json:"name"`
	Policy MinioAccessKeyPolicy `json:"policy,omitempty"`
	// populated by reconciler
	SecretKeyRef ResourceKeyRef `json:"secretKeyRef,omitempty"`
	// +kubebuilder:validation:Required
	SecretName string `json:"secretName"`
	// +kubebuilder:validation:Required
	TenantRef ResourceRef `json:"tenantRef"`
	// +kubebuilder:validation:Required
	User MinioAccessKeyIdentity `json:"user,omitempty"`
}

// MinioAccessKeyStatus defines the current state of MinioAccessKey
type MinioAccessKeyStatus struct {
	Synced      *bool               `json:"synced,omitempty"`
	CurrentSpec *MinioAccessKeySpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioAccessKeyList contains a list of MinioAccessKey
type MinioAccessKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioAccessKey `json:"items"`
}
