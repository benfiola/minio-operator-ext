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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +groupName=bfiola.dev

// SchemeGroupVersion is group version used to register these objects
var SchemeGroupVersion = schema.GroupVersion{Group: "bfiola.dev", Version: "v1"}

// Kind takes an unqualified kind and returns back a Group qualified GroupKind
func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

var (
	// SchemeBuilder initializes a scheme builder
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme is a global function that registers this API group & version to a scheme
	AddToScheme = SchemeBuilder.AddToScheme
)

// Adds the list of known types to Scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&MinioBucket{},
		&MinioBucketList{},
		&MinioGroup{},
		&MinioGroupList{},
		&MinioGroupBinding{},
		&MinioGroupBindingList{},
		&MinioPolicy{},
		&MinioPolicyList{},
		&MinioPolicyBinding{},
		&MinioPolicyBindingList{},
		&MinioUser{},
		&MinioUserList{},
		&MinioAccessKey{},
		&MinioAccessKeyList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// ResourceRef defines a reference to another kubernetes resource
type ResourceRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (rr ResourceRef) SetDefaultNamespace(ns string) ResourceRef {
	rrns := rr.Namespace
	if rrns == "" {
		rrns = ns
	}
	return ResourceRef{
		Name:      rr.Name,
		Namespace: rrns,
	}
}

// ResourceKeyRef defines a reference to a key of another kubernetes resource
type ResourceKeyRef struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (rkr ResourceKeyRef) SetDefaultNamespace(ns string) ResourceKeyRef {
	rkrns := rkr.Namespace
	if rkrns == "" {
		rkrns = ns
	}
	return ResourceKeyRef{
		Key:       rkr.Key,
		Name:      rkr.Name,
		Namespace: rkrns,
	}
}
