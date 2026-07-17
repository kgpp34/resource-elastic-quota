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

package allocation

import (
	"testing"
	"time"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
	"kgpp34.com/resource-elastic-quota/internal/quota"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCalculateBudgetsAndWeightedMaxMin(t *testing.T) {
	pool := testPool("100Gi", "10Gi", "1.5", "1.2")
	departments := []quotav1alpha1.DepartmentQuota{
		testDepartment("a", 1, "20Gi", "100Gi", "40Gi"),
		testDepartment("b", 3, "20Gi", "100Gi", "80Gi"),
	}
	snapshot := testSnapshot(t, pool, departments, false)

	plan, err := Calculate(testPolicy(), []quotav1alpha1.ResourcePool{pool}, departments, snapshot, time.Now())
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	poolResult, exists := plan.Pool("pool")
	if !exists {
		t.Fatal("pool result does not exist")
	}
	assertMemory(t, poolResult.PhysicalCapacity, "90Gi")
	assertMemory(t, poolResult.AdmissionBudget, "135Gi")
	assertMemory(t, poolResult.ConfiguredEntitlementBudget, "108Gi")
	assertMemory(t, poolResult.EffectiveEntitlementBudget, "108Gi")

	departmentA, _ := plan.Department("a", "pool")
	departmentB, _ := plan.Department("b", "pool")
	assertMemory(t, departmentA.EffectiveQuota, "37Gi")
	assertMemory(t, departmentB.EffectiveQuota, "71Gi")
	assertMemory(t, departmentA.Borrowed, "17Gi")
	assertMemory(t, departmentB.Borrowed, "51Gi")
}

func TestCalculateRaisesEntitlementToHistoricalBase(t *testing.T) {
	pool := testPool("40Gi", "0", "1", "1")
	departments := []quotav1alpha1.DepartmentQuota{
		testDepartment("a", 1, "30Gi", "40Gi", "0"),
		testDepartment("b", 1, "30Gi", "40Gi", "0"),
	}
	plan, err := Calculate(testPolicy(), []quotav1alpha1.ResourcePool{pool}, departments, testSnapshot(t, pool, departments, false), time.Now())
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	result, _ := plan.Pool("pool")
	assertMemory(t, result.ConfiguredEntitlementBudget, "40Gi")
	assertMemory(t, result.EffectiveEntitlementBudget, "60Gi")
	if !result.BaseQuotaOversubscribed {
		t.Fatal("BaseQuotaOversubscribed = false, want true")
	}
}

func TestCalculateDetectsAdmissionOverCapacity(t *testing.T) {
	pool := testPool("100Gi", "0", "0.5", "1")
	department := testDepartment("a", 1, "10Gi", "100Gi", "60Gi")
	plan, err := Calculate(testPolicy(), []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), time.Now())
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	result, _ := plan.Pool("pool")
	assertMemory(t, result.AdmissionBudget, "50Gi")
	if !result.AdmissionOverCapacity {
		t.Fatal("AdmissionOverCapacity = false, want true")
	}
}

func TestCalculateScaleUpAndScaleDown(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	pool := testPool("100Gi", "0", "1", "1")
	policy := testPolicy()
	policy.Spec.Allocation.MaxIncreasePercent = 10
	policy.Spec.Allocation.MaxDecreasePercent = 25
	policy.Spec.Allocation.Cooldown = metav1.Duration{Duration: 30 * time.Minute}

	t.Run("scale up is capped", func(t *testing.T) {
		department := testDepartment("a", 1, "10Gi", "100Gi", "80Gi")
		department.Status.Pools = []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name:            "pool",
			AllocatedLimits: memory("80Gi"),
			EffectiveQuota:  memory("40Gi"),
		}}
		plan, err := Calculate(policy, []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), now)
		if err != nil {
			t.Fatalf("Calculate() returned an error: %v", err)
		}
		result, _ := plan.Department("a", "pool")
		assertMemory(t, result.EffectiveQuota, "44Gi")
		if !result.OverQuota {
			t.Fatal("OverQuota = false, want true while gradual scale-up trails allocation")
		}
	})

	t.Run("minimum increase raises a small percentage step", func(t *testing.T) {
		department := testDepartment("a", 1, "10Gi", "100Gi", "80Gi")
		department.Status.Pools = []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name:            "pool",
			AllocatedLimits: memory("80Gi"),
			EffectiveQuota:  memory("40Gi"),
		}}
		policyWithMinimum := policy
		policyWithMinimum.Spec.Allocation.MinIncrease = memory("10Gi")
		plan, err := Calculate(policyWithMinimum, []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), now)
		if err != nil {
			t.Fatalf("Calculate() returned an error: %v", err)
		}
		result, _ := plan.Department("a", "pool")
		assertMemory(t, result.EffectiveQuota, "50Gi")
	})

	t.Run("idle quota is reclaimed by one step", func(t *testing.T) {
		department := testDepartment("a", 1, "10Gi", "100Gi", "20Gi")
		department.Spec.Pools[0].ReclaimAfter = metav1.Duration{Duration: time.Hour}
		old := metav1.NewTime(now.Add(-2 * time.Hour))
		department.Status.Pools = []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name:            "pool",
			AllocatedLimits: memory("20Gi"),
			EffectiveQuota:  memory("80Gi"),
			LastDemandTime:  &old,
			LastUpdateTime:  &old,
		}}
		plan, err := Calculate(policy, []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), now)
		if err != nil {
			t.Fatalf("Calculate() returned an error: %v", err)
		}
		result, _ := plan.Department("a", "pool")
		assertMemory(t, result.EffectiveQuota, "60Gi")
		if result.LastUpdateTime == nil || !result.LastUpdateTime.Time.Equal(now) {
			t.Fatalf("LastUpdateTime = %v, want %v", result.LastUpdateTime, now)
		}
	})

	t.Run("recent demand blocks reclaim", func(t *testing.T) {
		department := testDepartment("a", 1, "10Gi", "100Gi", "20Gi")
		department.Spec.Pools[0].ReclaimAfter = metav1.Duration{Duration: time.Hour}
		recent := metav1.NewTime(now.Add(-10 * time.Minute))
		oldUpdate := metav1.NewTime(now.Add(-2 * time.Hour))
		department.Status.Pools = []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name:            "pool",
			AllocatedLimits: memory("20Gi"),
			EffectiveQuota:  memory("80Gi"),
			LastDemandTime:  &recent,
			LastUpdateTime:  &oldUpdate,
		}}
		plan, err := Calculate(policy, []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), now)
		if err != nil {
			t.Fatalf("Calculate() returned an error: %v", err)
		}
		result, _ := plan.Department("a", "pool")
		assertMemory(t, result.EffectiveQuota, "80Gi")
	})
}

func TestCalculateCapacityReductionOverridesDelayedScaleDown(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	pool := testPool("40Gi", "0", "1", "1")
	departments := []quotav1alpha1.DepartmentQuota{
		testDepartment("a", 1, "10Gi", "100Gi", "10Gi"),
		testDepartment("b", 1, "10Gi", "100Gi", "10Gi"),
	}
	for i := range departments {
		departments[i].Spec.Pools[0].ReclaimAfter = metav1.Duration{Duration: time.Hour}
		recent := metav1.NewTime(now)
		departments[i].Status.Pools = []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name:            "pool",
			AllocatedLimits: memory("10Gi"),
			EffectiveQuota:  memory("30Gi"),
			LastDemandTime:  &recent,
			LastUpdateTime:  &recent,
		}}
	}
	plan, err := Calculate(testPolicy(), []quotav1alpha1.ResourcePool{pool}, departments, testSnapshot(t, pool, departments, false), now)
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	departmentA, _ := plan.Department("a", "pool")
	departmentB, _ := plan.Department("b", "pool")
	assertMemory(t, departmentA.EffectiveQuota, "20Gi")
	assertMemory(t, departmentB.EffectiveQuota, "20Gi")
}

func TestCalculateMemoryPressureSuspendsHeadroom(t *testing.T) {
	pool := testPool("100Gi", "0", "1", "1")
	department := testDepartment("a", 1, "10Gi", "100Gi", "40Gi")
	department.Spec.Pools[0].TargetUtilizationPercent = 50
	department.Spec.Pools[0].GrowthHeadroomPercent = 100

	plan, err := Calculate(testPolicy(), []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, true), time.Now())
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	poolResult, _ := plan.Pool("pool")
	if !poolResult.MemoryPressure {
		t.Fatal("MemoryPressure = false, want true")
	}
	departmentResult, _ := plan.Department("a", "pool")
	assertMemory(t, departmentResult.EffectiveQuota, "40Gi")
}

func TestCalculatePressureRecoveryWindowKeepsBorrowingBlocked(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	pool := testPool("100Gi", "0", "1", "1")
	transition := metav1.NewTime(now.Add(-2 * time.Minute))
	pool.Status.Conditions = []metav1.Condition{{
		Type:               "MemoryPressure",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: transition,
	}}
	department := testDepartment("a", 1, "10Gi", "100Gi", "40Gi")
	department.Spec.Pools[0].TargetUtilizationPercent = 50
	department.Spec.Pools[0].GrowthHeadroomPercent = 100
	policy := testPolicy()
	policy.Spec.Pressure.RecoveryWindow = metav1.Duration{Duration: 5 * time.Minute}

	plan, err := Calculate(policy, []quotav1alpha1.ResourcePool{pool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, pool, []quotav1alpha1.DepartmentQuota{department}, false), now)
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	poolResult, _ := plan.Pool("pool")
	if poolResult.MemoryPressure {
		t.Fatal("MemoryPressure = true, want false after node recovery")
	}
	if !poolResult.BorrowingBlocked {
		t.Fatal("BorrowingBlocked = false, want true during recovery window")
	}
	departmentResult, _ := plan.Department("a", "pool")
	assertMemory(t, departmentResult.EffectiveQuota, "40Gi")

	recoveredPool := pool
	recoveredPool.Status.Conditions = []metav1.Condition{
		{Type: "MemoryPressure", Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(now)},
		{Type: "BorrowingBlocked", Status: metav1.ConditionTrue, LastTransitionTime: transition},
	}
	plan, err = Calculate(policy, []quotav1alpha1.ResourcePool{recoveredPool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, recoveredPool, []quotav1alpha1.DepartmentQuota{department}, false), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Calculate() during recovery window returned an error: %v", err)
	}
	poolResult, _ = plan.Pool("pool")
	if !poolResult.BorrowingBlocked {
		t.Fatal("BorrowingBlocked = false, want true before recovery window expires")
	}
	plan, err = Calculate(policy, []quotav1alpha1.ResourcePool{recoveredPool}, []quotav1alpha1.DepartmentQuota{department}, testSnapshot(t, recoveredPool, []quotav1alpha1.DepartmentQuota{department}, false), now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("Calculate() after recovery window returned an error: %v", err)
	}
	poolResult, _ = plan.Pool("pool")
	if poolResult.BorrowingBlocked {
		t.Fatal("BorrowingBlocked = true, want false after recovery window")
	}
}

func testPolicy() quotav1alpha1.ElasticQuotaPolicy {
	return quotav1alpha1.ElasticQuotaPolicy{Spec: quotav1alpha1.ElasticQuotaPolicySpec{
		Resources: []corev1.ResourceName{corev1.ResourceMemory},
		Allocation: quotav1alpha1.AllocationPolicy{
			MaxIncreasePercent: 10,
			MaxDecreasePercent: 10,
		},
		Pressure: quotav1alpha1.PressurePolicy{
			BlockBorrowingOnNodeMemoryPressure: true,
		},
	}}
}

func testPool(allocatable, reserve, admissionRatio, entitlementRatio string) quotav1alpha1.ResourcePool {
	return quotav1alpha1.ResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: quotav1alpha1.ResourcePoolSpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"pool": "test"}},
			CapacityPolicy: quotav1alpha1.CapacityPolicy{
				Reserve:          memory(reserve),
				AdmissionRatio:   memory(admissionRatio),
				EntitlementRatio: memory(entitlementRatio),
			},
		},
		Status: quotav1alpha1.ResourcePoolStatus{Allocatable: memory(allocatable)},
	}
}

func testDepartment(name string, weight int32, base, maximum, allocated string) quotav1alpha1.DepartmentQuota {
	return quotav1alpha1.DepartmentQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: quotav1alpha1.DepartmentQuotaSpec{
			Department: name,
			Pools: []quotav1alpha1.DepartmentPoolQuota{{
				Name:                     "pool",
				BaseQuota:                memory(base),
				MaxQuota:                 memory(maximum),
				Weight:                   weight,
				TargetUtilizationPercent: 100,
			}},
		},
		Status: quotav1alpha1.DepartmentQuotaStatus{
			Pools: []quotav1alpha1.DepartmentPoolQuotaStatus{{
				Name:            "allocated-fixture",
				AllocatedLimits: memory(allocated),
			}},
		},
	}
}

func testSnapshot(
	t *testing.T,
	pool quotav1alpha1.ResourcePool,
	departments []quotav1alpha1.DepartmentQuota,
	memoryPressure bool,
) accounting.Snapshot {
	t.Helper()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{"pool": "test"}},
		Status: corev1.NodeStatus{
			Allocatable: pool.Status.Allocatable,
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: conditionStatus(memoryPressure)},
			},
		},
	}
	var namespaces []corev1.Namespace
	var pods []corev1.Pod
	for _, department := range departments {
		namespaceName := department.Name + "-apps"
		namespaces = append(namespaces, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			Labels: map[string]string{
				accounting.NamespaceEnforcedLabel:   "true",
				accounting.NamespaceDepartmentLabel: department.Spec.Department,
			},
		}})
		allocated := department.Status.Pools[0].AllocatedLimits
		pods = append(pods, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespaceName, Name: "pod", Labels: map[string]string{accounting.PodResourcePoolLabel: "pool"}},
			Spec: corev1.PodSpec{
				NodeName: "node",
				Containers: []corev1.Container{{
					Name:      "app",
					Resources: corev1.ResourceRequirements{Limits: allocated},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	snapshot, err := accounting.Calculate([]quotav1alpha1.ResourcePool{pool}, []corev1.Node{node}, namespaces, pods)
	if err != nil {
		t.Fatalf("accounting.Calculate() returned an error: %v", err)
	}
	return snapshot
}

func memory(value string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(value)}
}

func conditionStatus(value bool) corev1.ConditionStatus {
	if value {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func assertMemory(t *testing.T, actual corev1.ResourceList, expected string) {
	t.Helper()
	if !quota.Equal(actual, memory(expected)) {
		t.Fatalf("memory = %v, want %s", actual, expected)
	}
}
