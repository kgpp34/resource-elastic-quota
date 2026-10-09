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

// Package observability owns the controller's bounded-cardinality metrics.
package observability

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

const namespace = "resource_elastic_quota"

// Metrics is safe for concurrent controller and webhook use. Labels are
// deliberately limited to configured departments, pools, and fixed enums;
// Pods, Namespaces, object names, and UIDs are never labels.
type Metrics struct {
	poolAllocatable  *prometheus.GaugeVec
	poolAdmission    *prometheus.GaugeVec
	poolAllocated    *prometheus.GaugeVec
	poolObserved     *prometheus.GaugeVec
	departmentBase   *prometheus.GaugeVec
	departmentMax    *prometheus.GaugeVec
	departmentQuota  *prometheus.GaugeVec
	departmentLimits *prometheus.GaugeVec
	departmentUsage  *prometheus.GaugeVec
	departmentBorrow *prometheus.GaugeVec
	classification   *prometheus.GaugeVec
	metricsHealthy   prometheus.Gauge
	sampleAge        prometheus.Gauge
	admissions       *prometheus.CounterVec
	reconciles       *prometheus.CounterVec
	reconcileSeconds *prometheus.HistogramVec
	lastCalculation  int64
}

func New(registerer prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		poolAllocatable:  newMemoryGauge("pool_allocatable_memory_bytes", "Pool allocatable memory.", []string{"pool"}),
		poolAdmission:    newMemoryGauge("pool_admission_budget_memory_bytes", "Pool admission budget after overcommit.", []string{"pool"}),
		poolAllocated:    newMemoryGauge("pool_allocated_limits_memory_bytes", "Sum of admitted Pod memory limits in a pool.", []string{"pool"}),
		poolObserved:     newMemoryGauge("pool_observed_usage_memory_bytes", "Metrics API observed memory usage in a pool.", []string{"pool"}),
		departmentBase:   newMemoryGauge("department_base_quota_memory_bytes", "Configured department base quota.", []string{"department", "pool"}),
		departmentMax:    newMemoryGauge("department_max_quota_memory_bytes", "Configured department maximum quota.", []string{"department", "pool"}),
		departmentQuota:  newMemoryGauge("department_effective_quota_memory_bytes", "Current effective quota.", []string{"department", "pool"}),
		departmentLimits: newMemoryGauge("department_allocated_limits_memory_bytes", "Admitted memory limits.", []string{"department", "pool"}),
		departmentUsage:  newMemoryGauge("department_observed_usage_memory_bytes", "Metrics API observed memory usage.", []string{"department", "pool"}),
		departmentBorrow: newMemoryGauge("department_borrowed_memory_bytes", "Quota borrowed above base quota.", []string{"department", "pool"}),
		classification:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "classification_issues", Help: "Objects that could not be classified safely."}, []string{"reason"}),
		metricsHealthy:   prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "metrics_api_healthy", Help: "1 when the latest Metrics API refresh succeeded, otherwise 0."}),
		sampleAge:        prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "metrics_sample_age_seconds", Help: "Age of the newest included Pod metrics sample."}),
		admissions:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "admission_requests_total", Help: "Admission decisions by mode, result, and fixed reason."}, []string{"mode", "result", "reason"}),
		reconciles:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "reconcile_total", Help: "Policy reconciliation attempts."}, []string{"result"}),
		reconcileSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "reconcile_duration_seconds", Help: "Policy reconciliation duration.", Buckets: prometheus.DefBuckets}, []string{"result"}),
	}
	collectors := []prometheus.Collector{
		m.poolAllocatable, m.poolAdmission, m.poolAllocated, m.poolObserved,
		m.departmentBase, m.departmentMax, m.departmentQuota, m.departmentLimits,
		m.departmentUsage, m.departmentBorrow, m.classification, m.metricsHealthy,
		m.sampleAge, m.admissions, m.reconciles, m.reconcileSeconds,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: namespace, Name: "snapshot_age_seconds", Help: "Age of the latest successful accounting snapshot."}, func() float64 {
			last := atomic.LoadInt64(&m.lastCalculation)
			if last == 0 {
				return -1
			}
			return time.Since(time.Unix(0, last)).Seconds()
		}),
	}
	for _, collector := range collectors {
		if err := registerer.Register(collector); err != nil {
			return nil, fmt.Errorf("register quota metric: %w", err)
		}
	}
	return m, nil
}

func newMemoryGauge(name, help string, labels []string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

// ObserveReconcile records a completed reconcile attempt.
func (m *Metrics) ObserveReconcile(start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	m.reconciles.WithLabelValues(result).Inc()
	m.reconcileSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// ObserveAdmission implements webhook.AdmissionRecorder.
func (m *Metrics) ObserveAdmission(mode, result, reason string) {
	m.admissions.WithLabelValues(mode, result, reason).Inc()
}

// Publish replaces all current-state gauges after a successful reconciliation.
func (m *Metrics) Publish(
	pools []quotav1alpha1.ResourcePool,
	departments []quotav1alpha1.DepartmentQuota,
	poolUsage map[string]corev1.ResourceList,
	diagnostics accounting.Diagnostics,
	metricsHealthy bool,
	newestSample *time.Time,
	now time.Time,
) {
	for _, gauge := range []*prometheus.GaugeVec{m.poolAllocatable, m.poolAdmission, m.poolAllocated, m.poolObserved} {
		gauge.Reset()
	}
	for _, gauge := range []*prometheus.GaugeVec{m.departmentBase, m.departmentMax, m.departmentQuota, m.departmentLimits, m.departmentUsage, m.departmentBorrow} {
		gauge.Reset()
	}
	for i := range pools {
		pool := pools[i]
		setMemory(m.poolAllocatable.WithLabelValues(pool.Name), pool.Status.Allocatable)
		setMemory(m.poolAdmission.WithLabelValues(pool.Name), pool.Status.AdmissionBudget)
		setMemory(m.poolAllocated.WithLabelValues(pool.Name), pool.Status.AllocatedLimits)
		setMemory(m.poolObserved.WithLabelValues(pool.Name), poolUsage[pool.Name])
	}
	for i := range departments {
		department := departments[i]
		configured := make(map[string]quotav1alpha1.DepartmentPoolQuota, len(department.Spec.Pools))
		for _, pool := range department.Spec.Pools {
			configured[pool.Name] = pool
		}
		for _, status := range department.Status.Pools {
			labels := []string{department.Spec.Department, status.Name}
			config := configured[status.Name]
			setMemory(m.departmentBase.WithLabelValues(labels...), config.BaseQuota)
			setMemory(m.departmentMax.WithLabelValues(labels...), config.MaxQuota)
			setMemory(m.departmentQuota.WithLabelValues(labels...), status.EffectiveQuota)
			setMemory(m.departmentLimits.WithLabelValues(labels...), status.AllocatedLimits)
			setMemory(m.departmentUsage.WithLabelValues(labels...), status.ObservedUsage)
			setMemory(m.departmentBorrow.WithLabelValues(labels...), status.Borrowed)
		}
	}
	m.classification.Reset()
	m.classification.WithLabelValues("unmatched_nodes").Set(float64(diagnostics.UnmatchedNodes))
	m.classification.WithLabelValues("overlapping_nodes").Set(float64(diagnostics.OverlappingNodes))
	m.classification.WithLabelValues("invalid_namespaces").Set(float64(diagnostics.InvalidNamespaces))
	m.classification.WithLabelValues("unresolved_pods").Set(float64(diagnostics.UnresolvedPods))
	m.classification.WithLabelValues("pool_mismatches").Set(float64(diagnostics.DeclaredPoolMismatches))
	if metricsHealthy {
		m.metricsHealthy.Set(1)
	} else {
		m.metricsHealthy.Set(0)
	}
	if newestSample == nil {
		m.sampleAge.Set(-1)
	} else {
		m.sampleAge.Set(now.Sub(*newestSample).Seconds())
	}
	atomic.StoreInt64(&m.lastCalculation, now.UnixNano())
}

func setMemory(gauge prometheus.Gauge, resources corev1.ResourceList) {
	quantity, exists := resources[corev1.ResourceMemory]
	if !exists {
		gauge.Set(0)
		return
	}
	gauge.Set(float64(quantity.Value()))
}
