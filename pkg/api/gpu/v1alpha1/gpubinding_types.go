/*
Copyright 2025.

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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GPUBindingSpec struct {
	UUID        string                `json:"uuid"`
	AppName     string                `json:"appName"`
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
	Memory      *resource.Quantity    `json:"memory,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:rbac:groups=gpu.bytetrade.io,resources=gpubindings,verbs=get;list;watch;create;update;patch;delete

type GPUBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GPUBindingSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// GPUBindingList contains a list of GPUBinding.
type GPUBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUBinding{}, &GPUBindingList{})
}
