/*
Copyright kgpp34 2026.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CapacityPolicy controls how much of a pool can be admitted and allocated.
// Ratios are decimal quantities; for example, 1.2 means 120 percent.
type CapacityPolicy struct {
	Reserve          corev1.ResourceList `json:"reserve,omitempty"`
	AdmissionRatio   corev1.ResourceList `json:"admissionRatio,omitempty"`
	EntitlementRatio corev1.ResourceList `json:"entitlementRatio,omitempty"`
}

// ResourcePoolSpec defines a group of nodes with homogeneous scheduling traits.
type ResourcePoolSpec struct {
	NodeSelector   metav1.LabelSelector `json:"nodeSelector"`
	CapacityPolicy CapacityPolicy       `json:"capacityPolicy,omitempty"`
}

// ResourcePoolStatus describes the latest computed pool capacity and allocation.
type ResourcePoolStatus struct {
	ObservedGeneration          int64               `json:"observedGeneration,omitempty"`
	ReadyNodes                  int32               `json:"readyNodes,omitempty"`
	Allocatable                 corev1.ResourceList `json:"allocatable,omitempty"`
	AdmissionBudget             corev1.ResourceList `json:"admissionBudget,omitempty"`
	ConfiguredEntitlementBudget corev1.ResourceList `json:"configuredEntitlementBudget,omitempty"`
	EffectiveEntitlementBudget  corev1.ResourceList `json:"effectiveEntitlementBudget,omitempty"`
	AllocatedLimits             corev1.ResourceList `json:"allocatedLimits,omitempty"`
	Conditions                  []metav1.Condition  `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rpool
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.allocatable.memory`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourcePool is the Schema for the resourcepools API.
type ResourcePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePoolSpec   `json:"spec,omitempty"`
	Status ResourcePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourcePoolList contains a list of ResourcePool.
type ResourcePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourcePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourcePool{}, &ResourcePoolList{})
}
