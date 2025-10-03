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

// MinioServiceAccount defines a MinIO builtin service account identity.
type MinioServiceAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioServiceAccountSpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioServiceAccountStatus `json:"status,omitempty"`
}

// MinioServiceAccountSpec defines the desired state of MinioServiceAccount
type MinioServiceAccountSpec struct {
	Migrate bool `json:"migrate,omitempty"`

	// TODO: add policy
	// Policy     json.RawMessage `json:"policy,omitempty"` // Parsed value from iam/policy.Parse()

	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`

	// +kubebuilder:validation:Required
	TargetUser string `json:"targetUser,omitempty"`

	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`

	// +kubebuilder:validation:Required
	TargetSecretName string `json:"targetSecretName"`

	// TODO: add expiration
	// Expiration *time.Time `json:"expiration,omitempty"`

	TenantRef ResourceRef `json:"tenantRef"`
}

// MinioServiceAccountStatus defines the current state of MinioServiceAccount
type MinioServiceAccountStatus struct {
	Synced      *bool                    `json:"synced,omitempty"`
	CurrentSpec *MinioServiceAccountSpec `json:"currentSpec,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MinioServiceAccountList contains a list of MinioServiceAccount
type MinioServiceAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MinioServiceAccount `json:"items"`
}
