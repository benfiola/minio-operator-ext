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
// +k8s:deepcopy-gen=package

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	MinioBucketDeletionPolicyAlways  = "Always"
	MinioBucketDeletionPolicyIfEmpty = "IfEmpty"
)

// +genclient
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,description="Tenant Namespace",JSONPath=`.status.currentSpec.tenantRef.namespace`
// +kubebuilder:printcolumn:name="Tenant Name",type=string,description="Tenant Name",JSONPath=`.status.currentSpec.tenantRef.name`
// +kubebuilder:printcolumn:name="Name",type=string,description="Name",JSONPath=`.status.currentSpec.name`

// MinioBucket defines a MinIO tenant bucket.
type MinioBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MinioBucketSpec `json:"spec"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Status MinioBucketStatus `json:"status,omitempty"`
}

// MinioBucketSpec defines the desired state of MinioBucket
type MinioBucketSpec struct {
	DeletionPolicy string      `json:"deletionPolicy"`
	Migrate        bool        `json:"migrate,omitempty"`
	Name           string      `json:"name"`
	TenantRef      ResourceRef `json:"tenantRef"`
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
