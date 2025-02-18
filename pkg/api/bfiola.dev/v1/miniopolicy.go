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
