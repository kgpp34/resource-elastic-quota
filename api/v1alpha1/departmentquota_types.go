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

// EvictionPolicy controls whether future reclaim logic may evict Pods.
// +kubebuilder:validation:Enum=Deny;Allow
type EvictionPolicy string

const (
	// EvictionPolicyDeny disables automatic Pod eviction.
	EvictionPolicyDeny EvictionPolicy = "Deny"
	// EvictionPolicyAllow permits eviction when the global policy also enables it.
	EvictionPolicyAllow EvictionPolicy = "Allow"
)

// DepartmentPoolQuota defines one department's entitlement in one resource pool.
type DepartmentPoolQuota struct {
	Name string `json:"name"`

	BaseQuota corev1.ResourceList `json:"baseQuota"`
	MaxQuota  corev1.ResourceList `json:"maxQuota"`

	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	Weight int32 `json:"weight,omitempty"`

	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetUtilizationPercent int32 `json:"targetUtilizationPercent,omitempty"`

	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=0
	GrowthHeadroomPercent int32 `json:"growthHeadroomPercent,omitempty"`

	ReclaimAfter metav1.Duration `json:"reclaimAfter,omitempty"`
}

// DepartmentQuotaSpec defines quotas for the business namespaces of one department.
type DepartmentQuotaSpec struct {
	// +kubebuilder:validation:MinLength=1
	Department string `json:"department"`

	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// +kubebuilder:default=Deny
	EvictionPolicy EvictionPolicy `json:"evictionPolicy,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Pools []DepartmentPoolQuota `json:"pools"`
}

// DepartmentPoolQuotaStatus records the allocation and usage in one pool.
type DepartmentPoolQuotaStatus struct {
	Name string `json:"name"`

	AllocatedLimits corev1.ResourceList `json:"allocatedLimits,omitempty"`
	EffectiveQuota  corev1.ResourceList `json:"effectiveQuota,omitempty"`
	ObservedUsage   corev1.ResourceList `json:"observedUsage,omitempty"`
	Borrowed        corev1.ResourceList `json:"borrowed,omitempty"`
	LastDemandTime  *metav1.Time        `json:"lastDemandTime,omitempty"`
	LastUpdateTime  *metav1.Time        `json:"lastUpdateTime,omitempty"`
}

// DepartmentQuotaStatus describes the latest quota allocation for a department.
type DepartmentQuotaStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=name
	Pools      []DepartmentPoolQuotaStatus `json:"pools,omitempty"`
	Conditions []metav1.Condition          `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dquota
// +kubebuilder:printcolumn:name="Department",type=string,JSONPath=`.spec.department`
// +kubebuilder:printcolumn:name="Eviction",type=string,JSONPath=`.spec.evictionPolicy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DepartmentQuota is the Schema for the departmentquotas API.
type DepartmentQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DepartmentQuotaSpec   `json:"spec,omitempty"`
	Status DepartmentQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DepartmentQuotaList contains a list of DepartmentQuota.
type DepartmentQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DepartmentQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DepartmentQuota{}, &DepartmentQuotaList{})
}
