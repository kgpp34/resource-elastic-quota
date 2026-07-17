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

// MetricsProvider identifies the source of actual memory usage.
// +kubebuilder:validation:Enum=MetricsAPI;Prometheus
type MetricsProvider string

const (
	// MetricsProviderMetricsAPI reads metrics.k8s.io, normally served by metrics-server.
	MetricsProviderMetricsAPI MetricsProvider = "MetricsAPI"
	// MetricsProviderPrometheus reads a configured Prometheus endpoint.
	MetricsProviderPrometheus MetricsProvider = "Prometheus"
)

// AdmissionMode controls whether quota violations are observed, warned, or denied.
// +kubebuilder:validation:Enum=Observe;Warn;Enforce
type AdmissionMode string

const (
	AdmissionModeObserve AdmissionMode = "Observe"
	AdmissionModeWarn    AdmissionMode = "Warn"
	AdmissionModeEnforce AdmissionMode = "Enforce"
)

// EvictionTrigger identifies the only condition allowed to start automatic reclaim.
// +kubebuilder:validation:Enum=SevereNodePressure
type EvictionTrigger string

const (
	EvictionTriggerSevereNodePressure EvictionTrigger = "SevereNodePressure"
)

// AllocationPolicy configures periodic quota calculation.
type AllocationPolicy struct {
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	MaxIncreasePercent int32 `json:"maxIncreasePercent,omitempty"`

	// MinIncrease prevents a small current quota from taking too many cycles to grow.
	// Version one accepts only limits.memory.
	MinIncrease corev1.ResourceList `json:"minIncrease,omitempty"`

	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	MaxDecreasePercent int32 `json:"maxDecreasePercent,omitempty"`

	Cooldown   metav1.Duration `json:"cooldown,omitempty"`
	FullResync metav1.Duration `json:"fullResync,omitempty"`
}

// PressurePolicy configures capacity protection thresholds.
type PressurePolicy struct {
	// +kubebuilder:default=true
	BlockBorrowingOnNodeMemoryPressure bool `json:"blockBorrowingOnNodeMemoryPressure,omitempty"`

	// +kubebuilder:default=85
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	HighWatermarkPercent int32 `json:"highWatermarkPercent,omitempty"`

	// +kubebuilder:default=95
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	SevereWatermarkPercent int32 `json:"severeWatermarkPercent,omitempty"`

	RecoveryWindow metav1.Duration `json:"recoveryWindow,omitempty"`
}

// MetricsPolicy configures actual memory usage collection.
type MetricsPolicy struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=MetricsAPI
	Provider MetricsProvider `json:"provider,omitempty"`

	CollectionInterval metav1.Duration `json:"collectionInterval,omitempty"`
	MaxSampleAge       metav1.Duration `json:"maxSampleAge,omitempty"`
}

// EvictionConfig keeps automatic eviction disabled unless explicitly enabled.
type EvictionConfig struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=SevereNodePressure
	Trigger EvictionTrigger `json:"trigger,omitempty"`

	SustainedFor metav1.Duration `json:"sustainedFor,omitempty"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MaxPodsPerCycle int32 `json:"maxPodsPerCycle,omitempty"`

	// +kubebuilder:default=true
	RespectPDB bool `json:"respectPDB,omitempty"`
}

// AdmissionPolicy configures the validating admission webhook behavior.
type AdmissionPolicy struct {
	// +kubebuilder:default=Observe
	Mode AdmissionMode `json:"mode,omitempty"`

	MaxSnapshotAge metav1.Duration `json:"maxSnapshotAge,omitempty"`

	// +kubebuilder:default=false
	FailClosedOnStaleSnapshot bool `json:"failClosedOnStaleSnapshot,omitempty"`

	// +kubebuilder:default=false
	FailClosedOnUnknownPool bool `json:"failClosedOnUnknownPool,omitempty"`
}

// ElasticQuotaPolicySpec defines the singleton cluster-wide control policy.
type ElasticQuotaPolicySpec struct {
	// Version one supports only memory. The list allows later resource expansion.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:items:Enum=memory
	Resources []corev1.ResourceName `json:"resources"`

	Allocation AllocationPolicy `json:"allocation,omitempty"`
	Pressure   PressurePolicy   `json:"pressure,omitempty"`
	Metrics    MetricsPolicy    `json:"metrics,omitempty"`
	Eviction   EvictionConfig   `json:"eviction,omitempty"`
	Admission  AdmissionPolicy  `json:"admission,omitempty"`
}

// ElasticQuotaPolicyStatus describes policy health and calculation freshness.
type ElasticQuotaPolicyStatus struct {
	ObservedGeneration  int64              `json:"observedGeneration,omitempty"`
	LastCalculationTime *metav1.Time       `json:"lastCalculationTime,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eqpolicy
// +kubebuilder:printcolumn:name="Admission",type=string,JSONPath=`.spec.admission.mode`
// +kubebuilder:printcolumn:name="Eviction",type=boolean,JSONPath=`.spec.eviction.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ElasticQuotaPolicy is the Schema for the elasticquotapolicies API.
type ElasticQuotaPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticQuotaPolicySpec   `json:"spec,omitempty"`
	Status ElasticQuotaPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ElasticQuotaPolicyList contains a list of ElasticQuotaPolicy.
type ElasticQuotaPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ElasticQuotaPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ElasticQuotaPolicy{}, &ElasticQuotaPolicyList{})
}
