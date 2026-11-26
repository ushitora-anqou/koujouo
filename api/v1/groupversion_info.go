/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1 contains API Schema definitions for the koujouo.anqou.net v1 API group.
// +kubebuilder:object:generate=true
// +groupName=koujouo.anqou.net
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// These globals follow the standard scheme registration pattern for
// Kubernetes API groups.
var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	//
	//nolint:gochecknoglobals // Standard scheme registration pattern.
	SchemeGroupVersion = schema.GroupVersion{Group: "koujouo.anqou.net", Version: "v1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	//
	//nolint:gochecknoglobals // Standard scheme registration pattern.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	//
	//nolint:gochecknoglobals // Standard scheme registration pattern.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)

		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	//
	//nolint:gochecknoglobals // Standard scheme registration pattern.
	AddToScheme = SchemeBuilder.AddToScheme
)
