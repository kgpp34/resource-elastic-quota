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

package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

func TestPublishExportsBoundedQuotaMetrics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics, err := New(registry)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	pools := []quotav1alpha1.ResourcePool{{
		ObjectMeta: metav1.ObjectMeta{Name: "amd"},
		Status: quotav1alpha1.ResourcePoolStatus{
			Allocatable:     memory("8Gi"),
			AdmissionBudget: memory("12Gi"),
			AllocatedLimits: memory("4Gi"),
		},
	}}
	departments := []quotav1alpha1.DepartmentQuota{{
		Spec: quotav1alpha1.DepartmentQuotaSpec{
			Department: "risk",
			Pools:      []quotav1alpha1.DepartmentPoolQuota{{Name: "amd", BaseQuota: memory("2Gi"), MaxQuota: memory("6Gi")}},
		},
		Status: quotav1alpha1.DepartmentQuotaStatus{Pools: []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name: "amd", EffectiveQuota: memory("5Gi"), AllocatedLimits: memory("4Gi"), ObservedUsage: memory("1Gi"), Borrowed: memory("3Gi"),
		}}},
	}}
	now := time.Now()
	metrics.Publish(pools, departments, map[string]corev1.ResourceList{"amd": memory("2Gi")}, accounting.Diagnostics{}, true, &now, now)
	metrics.ObserveAdmission("Enforce", "allowed", "within_quota")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	wanted := map[string]bool{
		"resource_elastic_quota_pool_allocatable_memory_bytes":          false,
		"resource_elastic_quota_department_observed_usage_memory_bytes": false,
		"resource_elastic_quota_admission_requests_total":               false,
		"resource_elastic_quota_snapshot_age_seconds":                   false,
	}
	for _, family := range families {
		if _, exists := wanted[family.GetName()]; exists {
			wanted[family.GetName()] = true
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "pod" || label.GetName() == "namespace" || label.GetName() == "uid" {
					t.Fatalf("metric %s contains forbidden high-cardinality label %s", family.GetName(), label.GetName())
				}
			}
		}
	}
	for name, found := range wanted {
		if !found {
			t.Errorf("metric %s was not gathered", name)
		}
	}
}

func memory(value string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(value)}
}
