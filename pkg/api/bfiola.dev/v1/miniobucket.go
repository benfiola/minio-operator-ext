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
	"encoding/xml"

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

// MinioBucketVersioningExcludedPrefix is effectively a copy of [minioclient.ExcludedPrefix].
type MinioBucketVersioningExcludedPrefix struct {
	Prefix string `json:"Prefix"`
}

// MinioBucketVersioningConfiguration is effectively a copy of [minioclient.BucketVersioningConfiguration].
type MinioBucketVersioningConfiguration struct {
	XMLName          xml.Name                              `xml:"VersioningConfiguration" json:"-"`
	Status           string                                `xml:"Status" json:"Status"`
	MFADelete        string                                `xml:"MfaDelete,omitempty" json:"MfaDelete,omitempty"`
	ExcludedPrefixes []MinioBucketVersioningExcludedPrefix `xml:"ExcludedPrefixes,omitempty" json:"ExcludedPrefixes,omitempty"`
	ExcludeFolders   bool                                  `xml:"ExcludeFolders,omitempty" json:"ExcludeFolders,omitempty"`
}

// MinioBucketLifecycleAbortIncompleteMultipartUpload is effectively a copy of [lifecycle.AbortIncompleteMultipartUpload].
type MinioBucketLifecycleAbortIncompleteMultipartUpload struct {
	XMLName             xml.Name `xml:"AbortIncompleteMultipartUpload,omitempty"  json:"-"`
	DaysAfterInitiation int      `xml:"DaysAfterInitiation,omitempty" json:"DaysAfterInitiation,omitempty"`
}

// MinioBucketLifecycleExpiration is effectively a copy of [lifecycle.Expiration].
type MinioBucketLifecycleExpiration struct {
	XMLName      xml.Name    `xml:"Expiration,omitempty" json:"-"`
	Date         metav1.Time `xml:"Date,omitempty" json:"Date,omitempty"`
	Days         int         `xml:"Days,omitempty" json:"Days,omitempty"`
	DeleteMarker bool        `xml:"ExpiredObjectDeleteMarker,omitempty" json:"ExpiredObjectDeleteMarker,omitempty"`
	DeleteAll    bool        `xml:"ExpiredObjectAllVersions,omitempty" json:"ExpiredObjectAllVersions,omitempty"`
}

// MinioBucketLifecycleDelMarkerExpiration is effectively a copy of [lifecycle.DelMarkerExpiration].
type MinioBucketLifecycleDelMarkerExpiration struct {
	XMLName xml.Name `xml:"DelMarkerExpiration" json:"-"`
	Days    int      `xml:"Days,omitempty" json:"Days,omitempty"`
}

// MinioBucketLifecycleTag is effectively a copy of [lifecycle.Tag].
type MinioBucketLifecycleTag struct {
	XMLName xml.Name `xml:"Tag,omitempty" json:"-"`
	Key     string   `xml:"Key,omitempty" json:"Key,omitempty"`
	Value   string   `xml:"Value,omitempty" json:"Value,omitempty"`
}

// MinioBucketLifecycleAnd is effectively a copy of [lifecycle.And].
type MinioBucketLifecycleAnd struct {
	XMLName               xml.Name                  `xml:"And" json:"-"`
	Prefix                string                    `xml:"Prefix" json:"Prefix,omitempty"`
	Tags                  []MinioBucketLifecycleTag `xml:"Tag" json:"Tags,omitempty"`
	ObjectSizeLessThan    int64                     `xml:"ObjectSizeLessThan,omitempty" json:"ObjectSizeLessThan,omitempty"`
	ObjectSizeGreaterThan int64                     `xml:"ObjectSizeGreaterThan,omitempty" json:"ObjectSizeGreaterThan,omitempty"`
}

// MinioBucketLifecycleFilter is effectively a copy of [lifecycle.Filter].
type MinioBucketLifecycleFilter struct {
	XMLName               xml.Name                `xml:"Filter" json:"-"`
	And                   MinioBucketLifecycleAnd `xml:"And,omitempty" json:"And,omitempty"`
	Prefix                string                  `xml:"Prefix,omitempty" json:"Prefix,omitempty"`
	Tag                   MinioBucketLifecycleTag `xml:"Tag,omitempty" json:"Tag,omitempty"`
	ObjectSizeLessThan    int64                   `xml:"ObjectSizeLessThan,omitempty" json:"ObjectSizeLessThan,omitempty"`
	ObjectSizeGreaterThan int64                   `xml:"ObjectSizeGreaterThan,omitempty" json:"ObjectSizeGreaterThan,omitempty"`
}

// MinioBucketLifecycleNoncurrentVersionExpiration is effectively a copy of [lifecycle.Expiration].
type MinioBucketLifecycleNoncurrentVersionExpiration struct {
	XMLName                 xml.Name `xml:"NoncurrentVersionExpiration" json:"-"`
	NoncurrentDays          int      `xml:"NoncurrentDays,omitempty" json:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions int      `xml:"NewerNoncurrentVersions,omitempty" json:"NewerNoncurrentVersions,omitempty"`
}

// MinioBucketLifecycleNoncurrentVersionTransition is effectively a copy of [lifecycle.NoncurrentVersionTransition].
type MinioBucketLifecycleNoncurrentVersionTransition struct {
	XMLName                 xml.Name `xml:"NoncurrentVersionTransition,omitempty"  json:"-"`
	StorageClass            string   `xml:"StorageClass,omitempty" json:"StorageClass,omitempty"`
	NoncurrentDays          int      `xml:"NoncurrentDays" json:"NoncurrentDays"`
	NewerNoncurrentVersions int      `xml:"NewerNoncurrentVersions,omitempty" json:"NewerNoncurrentVersions,omitempty"`
}

// MinioBucketLifecycleTransition is effectively a copy of [lifecycle.Transition].
type MinioBucketLifecycleTransition struct {
	XMLName      xml.Name    `xml:"Transition" json:"-"`
	Date         metav1.Time `xml:"Date,omitempty" json:"Date,omitempty"`
	StorageClass string      `xml:"StorageClass,omitempty" json:"StorageClass,omitempty"`
	Days         int         `xml:"Days" json:"Days"`
}

// MinioBucketLifecycleRule is effectively a copy of [lifecycle.Rule].
type MinioBucketLifecycleRule struct {
	XMLName                        xml.Name                                           `xml:"Rule,omitempty" json:"-"`
	AbortIncompleteMultipartUpload MinioBucketLifecycleAbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload,omitempty" json:"AbortIncompleteMultipartUpload,omitempty"`
	Expiration                     MinioBucketLifecycleExpiration                     `xml:"Expiration,omitempty" json:"Expiration,omitempty"`
	DelMarkerExpiration            MinioBucketLifecycleDelMarkerExpiration            `xml:"DelMarkerExpiration,omitempty" json:"DelMarkerExpiration,omitempty"`
	ID                             string                                             `xml:"ID" json:"ID"`
	RuleFilter                     MinioBucketLifecycleFilter                         `xml:"Filter,omitempty" json:"Filter,omitempty"`
	NoncurrentVersionExpiration    MinioBucketLifecycleNoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration,omitempty"  json:"NoncurrentVersionExpiration,omitempty"`
	NoncurrentVersionTransition    MinioBucketLifecycleNoncurrentVersionTransition    `xml:"NoncurrentVersionTransition,omitempty" json:"NoncurrentVersionTransition,omitempty"`
	Prefix                         string                                             `xml:"Prefix,omitempty" json:"Prefix,omitempty"`
	Status                         string                                             `xml:"Status" json:"Status"`
	Transition                     MinioBucketLifecycleTransition                     `xml:"Transition,omitempty" json:"Transition,omitempty"`
}

// MinioBucketLifecycleConfiguration is effectively a copy of [lifecycle.Configuration].
type MinioBucketLifecycleConfiguration struct {
	XMLName xml.Name                   `xml:"LifecycleConfiguration,omitempty" json:"-"`
	Rules   []MinioBucketLifecycleRule `xml:"Rule" json:"Rules"`
}

// MinioBucketSpec defines the desired state of MinioBucket
type MinioBucketSpec struct {
	DeletionPolicy string                              `json:"deletionPolicy"`
	Lifecycle      *MinioBucketLifecycleConfiguration  `json:"lifecycle,omitempty"`
	Migrate        bool                                `json:"migrate,omitempty"`
	Name           string                              `json:"name"`
	TenantRef      ResourceRef                         `json:"tenantRef"`
	Versioning     *MinioBucketVersioningConfiguration `json:"versioning,omitempty"`
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
