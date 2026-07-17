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

package controller

import (
	"context"
	"testing"
	"time"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
	"kgpp34.com/resource-elastic-quota/internal/allocation"
	"kgpp34.com/resource-elastic-quota/internal/quota"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestElasticQuotaPolicyReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := quotav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add quota scheme: %v", err)
	}

	fixedTime := time.Date(2026, time.July, 17, 8, 0, 0, 0, time.UTC)
	policy := &quotav1alpha1.ElasticQuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultPolicyName, Generation: 2},
		Spec: quotav1alpha1.ElasticQuotaPolicySpec{
			Resources: []corev1.ResourceName{corev1.ResourceMemory},
			Allocation: quotav1alpha1.AllocationPolicy{
				MaxIncreasePercent: 100,
				MaxDecreasePercent: 10,
			},
		},
	}
	resourcePool := &quotav1alpha1.ResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "arm", Generation: 3},
		Spec: quotav1alpha1.ResourcePoolSpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"arch": "arm64"}},
		},
	}
	departmentQuota := &quotav1alpha1.DepartmentQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "risk", Generation: 4},
		Spec: quotav1alpha1.DepartmentQuotaSpec{
			Department: "risk",
			Pools: []quotav1alpha1.DepartmentPoolQuota{{
				Name:                     "arm",
				BaseQuota:                corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				MaxQuota:                 corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				Weight:                   100,
				TargetUtilizationPercent: 100,
			}},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "risk-apps",
		Labels: map[string]string{
			accounting.NamespaceEnforcedLabel:   "true",
			accounting.NamespaceDepartmentLabel: "risk",
		},
	}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "arm-1", Labels: map[string]string{"arch": "arm64"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Gi")},
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	pod := testPod("risk-apps", "app-1", "arm-1", "2Gi")

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, resourcePool, departmentQuota, namespace, node, pod).
		Build()
	countedClient := &countingClient{Client: baseClient}
	reconciler := &ElasticQuotaPolicyReconciler{
		Client:             countedClient,
		FullResyncInterval: time.Minute,
		Now:                func() time.Time { return fixedTime },
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: DefaultPolicyName},
	})
	if err != nil {
		t.Fatalf("Reconcile() returned an error: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("Reconcile() requeue = %s, want %s", result.RequeueAfter, time.Minute)
	}
	if countedClient.statusPatches != 3 {
		t.Fatalf("status patches = %d, want 3", countedClient.statusPatches)
	}

	assertResourcePoolStatus(t, baseClient)
	assertDepartmentQuotaStatus(t, baseClient, "2Gi")
	assertPolicyStatus(t, baseClient, fixedTime)

	countedClient.statusPatches = 0
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: DefaultPolicyName},
	}); err != nil {
		t.Fatalf("second Reconcile() returned an error: %v", err)
	}
	if countedClient.statusPatches != 0 {
		t.Fatalf("unchanged reconciliation made %d status patches, want 0", countedClient.statusPatches)
	}

	secondPod := testPod("risk-apps", "app-2", "arm-1", "1Gi")
	if err := baseClient.Create(context.Background(), secondPod); err != nil {
		t.Fatalf("create second pod: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: DefaultPolicyName},
	}); err != nil {
		t.Fatalf("reconcile after pod creation: %v", err)
	}
	assertDepartmentQuotaStatus(t, baseClient, "3Gi")
}

func TestElasticQuotaPolicyReconcilerIgnoresUnknownPolicy(t *testing.T) {
	reconciler := &ElasticQuotaPolicyReconciler{}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "another-policy"},
	})
	if err != nil {
		t.Fatalf("Reconcile() returned an error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %+v, want empty result", result)
	}
}

func TestFullResyncIntervalUsesAllocationCadence(t *testing.T) {
	reconciler := &ElasticQuotaPolicyReconciler{}
	policy := &quotav1alpha1.ElasticQuotaPolicy{Spec: quotav1alpha1.ElasticQuotaPolicySpec{
		Allocation: quotav1alpha1.AllocationPolicy{
			IntervalSeconds: 30,
			FullResync:      metav1.Duration{Duration: 5 * time.Minute},
		},
	}}
	if actual := reconciler.fullResyncInterval(policy); actual != 30*time.Second {
		t.Fatalf("fullResyncInterval() = %s, want 30s", actual)
	}

	policy.Spec.Allocation.IntervalSeconds = 0
	if actual := reconciler.fullResyncInterval(policy); actual != 5*time.Minute {
		t.Fatalf("fullResyncInterval() = %s, want 5m", actual)
	}
}

func TestValidateNamespaceSelector(t *testing.T) {
	namespaces := []corev1.Namespace{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "risk-apps",
				Labels: map[string]string{
					accounting.NamespaceEnforcedLabel:   "true",
					accounting.NamespaceDepartmentLabel: "risk",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "trading-apps",
				Labels: map[string]string{
					accounting.NamespaceEnforcedLabel:   "true",
					accounting.NamespaceDepartmentLabel: "trading",
				},
			},
		},
	}
	tests := []struct {
		name       string
		selector   metav1.LabelSelector
		wantReason string
	}{
		{
			name: "valid selector",
			selector: metav1.LabelSelector{MatchLabels: map[string]string{
				accounting.NamespaceEnforcedLabel:   "true",
				accounting.NamespaceDepartmentLabel: "risk",
			}},
		},
		{
			name: "does not match department namespace",
			selector: metav1.LabelSelector{MatchLabels: map[string]string{
				accounting.NamespaceDepartmentLabel: "another",
			}},
			wantReason: "NamespaceSelectorMismatch",
		},
		{
			name:       "crosses departments",
			selector:   metav1.LabelSelector{MatchLabels: map[string]string{accounting.NamespaceEnforcedLabel: "true"}},
			wantReason: "NamespaceSelectorCrossesDepartments",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			departmentQuota := quotav1alpha1.DepartmentQuota{
				Spec: quotav1alpha1.DepartmentQuotaSpec{
					Department:        "risk",
					NamespaceSelector: test.selector,
				},
			}
			reason, _ := validateNamespaceSelector(departmentQuota, namespaces)
			if reason != test.wantReason {
				t.Fatalf("validateNamespaceSelector() reason = %q, want %q", reason, test.wantReason)
			}
		})
	}
}

func TestMergeDepartmentPoolStatusDropsStaleAndClearsUnplannedQuota(t *testing.T) {
	current := []quotav1alpha1.DepartmentPoolQuotaStatus{
		{Name: "configured", EffectiveQuota: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("10Gi")}},
		{Name: "stale", AllocatedLimits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
	}
	result := mergeDepartmentPoolStatus(
		current,
		"department",
		map[string]struct{}{"configured": {}},
		nil,
		allocation.Plan{},
	)
	if len(result) != 1 || result[0].Name != "configured" {
		t.Fatalf("mergeDepartmentPoolStatus() = %+v, want only configured pool", result)
	}
	if len(result[0].EffectiveQuota) != 0 {
		t.Fatalf("unplanned effective quota = %v, want empty", result[0].EffectiveQuota)
	}
}

type countingClient struct {
	client.Client
	statusPatches int
}

func (c *countingClient) Status() client.StatusWriter {
	return &countingStatusWriter{
		StatusWriter: c.Client.Status(),
		patches:      &c.statusPatches,
	}
}

type countingStatusWriter struct {
	client.StatusWriter
	patches *int
}

func (w *countingStatusWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	if err := w.StatusWriter.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	*w.patches++
	return nil
}

func testPod(namespace, name, nodeName, memory string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{accounting.PodResourcePoolLabel: "arm"},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func assertResourcePoolStatus(t *testing.T, kubeClient client.Client) {
	t.Helper()
	resourcePool := &quotav1alpha1.ResourcePool{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: "arm"}, resourcePool); err != nil {
		t.Fatalf("get resource pool: %v", err)
	}
	if resourcePool.Status.ReadyNodes != 1 {
		t.Fatalf("ready nodes = %d, want 1", resourcePool.Status.ReadyNodes)
	}
	assertStatusMemory(t, resourcePool.Status.Allocatable, "64Gi")
	assertStatusMemory(t, resourcePool.Status.AllocatedLimits, "2Gi")
	assertStatusMemory(t, resourcePool.Status.AdmissionBudget, "64Gi")
	assertStatusMemory(t, resourcePool.Status.ConfiguredEntitlementBudget, "64Gi")
	assertStatusMemory(t, resourcePool.Status.EffectiveEntitlementBudget, "64Gi")
	if !meta.IsStatusConditionTrue(resourcePool.Status.Conditions, "Ready") {
		t.Fatalf("resource pool Ready condition is not true: %+v", resourcePool.Status.Conditions)
	}
	for _, conditionType := range []string{"BaseQuotaOversubscribed", "AdmissionOverCapacity", "MemoryPressure", "BorrowingBlocked"} {
		if !meta.IsStatusConditionFalse(resourcePool.Status.Conditions, conditionType) {
			t.Fatalf("resource pool %s condition is not false: %+v", conditionType, resourcePool.Status.Conditions)
		}
	}
}

func assertDepartmentQuotaStatus(t *testing.T, kubeClient client.Client, expectedMemory string) {
	t.Helper()
	departmentQuota := &quotav1alpha1.DepartmentQuota{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: "risk"}, departmentQuota); err != nil {
		t.Fatalf("get department quota: %v", err)
	}
	if len(departmentQuota.Status.Pools) != 1 {
		t.Fatalf("department pool status count = %d, want 1", len(departmentQuota.Status.Pools))
	}
	assertStatusMemory(t, departmentQuota.Status.Pools[0].AllocatedLimits, expectedMemory)
	assertStatusMemory(t, departmentQuota.Status.Pools[0].EffectiveQuota, expectedMemory)
	assertStatusMemory(t, departmentQuota.Status.Pools[0].Borrowed, borrowedMemory(expectedMemory))
	if !meta.IsStatusConditionTrue(departmentQuota.Status.Conditions, "Ready") {
		t.Fatalf("department Ready condition is not true: %+v", departmentQuota.Status.Conditions)
	}
	if !meta.IsStatusConditionFalse(departmentQuota.Status.Conditions, "OverQuota") {
		t.Fatalf("department OverQuota condition is not false: %+v", departmentQuota.Status.Conditions)
	}
}

func borrowedMemory(allocated string) string {
	quantity := resource.MustParse(allocated)
	base := resource.MustParse("1Gi")
	quantity.Sub(base)
	return quantity.String()
}

func assertPolicyStatus(t *testing.T, kubeClient client.Client, expectedTime time.Time) {
	t.Helper()
	policy := &quotav1alpha1.ElasticQuotaPolicy{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: DefaultPolicyName}, policy); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if policy.Status.LastCalculationTime == nil || !policy.Status.LastCalculationTime.Time.Equal(expectedTime) {
		t.Fatalf("last calculation time = %v, want %v", policy.Status.LastCalculationTime, expectedTime)
	}
	if !meta.IsStatusConditionTrue(policy.Status.Conditions, "Ready") {
		t.Fatalf("policy Ready condition is not true: %+v", policy.Status.Conditions)
	}
}

func assertStatusMemory(t *testing.T, resources corev1.ResourceList, expected string) {
	t.Helper()
	want := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(expected)}
	if !quota.Equal(resources, want) {
		t.Fatalf("resources = %v, want %v", resources, want)
	}
}
