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

package accounting

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/quota"
)

func TestPodLimits(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "sum regular containers",
			pod: podWithResources(
				[]corev1.ResourceRequirements{limits("1Gi"), limits("2Gi")},
				nil,
				"",
			),
			want: "3Gi",
		},
		{
			name: "largest init container exceeds regular sum",
			pod: podWithResources(
				[]corev1.ResourceRequirements{limits("1Gi"), limits("2Gi")},
				[]corev1.ResourceRequirements{limits("4Gi"), limits("5Gi")},
				"",
			),
			want: "5Gi",
		},
		{
			name: "regular sum exceeds init containers",
			pod: podWithResources(
				[]corev1.ResourceRequirements{limits("3Gi"), limits("2Gi")},
				[]corev1.ResourceRequirements{limits("4Gi")},
				"",
			),
			want: "5Gi",
		},
		{
			name: "add pod overhead",
			pod: podWithResources(
				[]corev1.ResourceRequirements{limits("1Gi")},
				[]corev1.ResourceRequirements{limits("2Gi")},
				"512Mi",
			),
			want: "2560Mi",
		},
		{
			name: "ignore requests and cpu",
			pod: podWithResources(
				[]corev1.ResourceRequirements{{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("10Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("8"),
					},
				}},
				nil,
				"",
			),
			want: "0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := PodLimits(test.pod)[corev1.ResourceMemory]
			want := resource.MustParse(test.want)
			if got.Cmp(want) != 0 {
				t.Fatalf("PodLimits() memory = %s, want %s", got.String(), want.String())
			}
		})
	}
}

func TestShouldCountPod(t *testing.T) {
	now := metav1.NewTime(time.Now())
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{name: "pending", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, want: true},
		{name: "running", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, want: true},
		{name: "unknown", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, want: true},
		{
			name: "terminating",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: true,
		},
		{name: "succeeded", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}},
		{name: "failed", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ShouldCountPod(test.pod); got != test.want {
				t.Fatalf("ShouldCountPod() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCalculate(t *testing.T) {
	pools := []quotav1alpha1.ResourcePool{
		resourcePool("arm", map[string]string{"arch": "arm64", "os": "kylin"}),
		resourcePool("amd", map[string]string{"arch": "amd64", "os": "kylin"}),
	}
	nodes := []corev1.Node{
		node("arm-ready", map[string]string{"arch": "arm64", "os": "kylin"}, "64Gi", true, false),
		node("arm-cordoned", map[string]string{"arch": "arm64", "os": "kylin"}, "32Gi", true, true),
		node("amd-not-ready", map[string]string{"arch": "amd64", "os": "kylin"}, "128Gi", false, false),
	}
	namespaces := []corev1.Namespace{
		managedNamespace("business", "risk"),
		{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "invalid",
				Labels: map[string]string{NamespaceEnforcedLabel: "true"},
			},
		},
	}
	pods := []corev1.Pod{
		pod("business", "running", "arm-ready", "amd", "2Gi", corev1.PodRunning),
		pod("business", "pending", "", "amd", "3Gi", corev1.PodPending),
		pod("business", "terminating", "arm-cordoned", "arm", "4Gi", corev1.PodRunning),
		pod("system", "system", "arm-ready", "", "1Gi", corev1.PodRunning),
		pod("business", "mismatch", "arm-ready", "amd", "5Gi", corev1.PodRunning),
		pod("business", "fallback", "deleted-node", "amd", "6Gi", corev1.PodUnknown),
		pod("business", "not-ready", "amd-not-ready", "amd", "2Gi", corev1.PodRunning),
		pod("business", "unresolved", "", "", "7Gi", corev1.PodPending),
		pod("business", "finished", "arm-ready", "arm", "20Gi", corev1.PodSucceeded),
	}
	deletionTime := metav1.NewTime(time.Now())
	pods[2].DeletionTimestamp = &deletionTime

	snapshot, err := Calculate(pools, nodes, namespaces, pods)
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}

	calculatedPools := poolsByName(snapshot.Pools())
	assertPool(t, calculatedPools["arm"], 1, "64Gi", "12Gi")
	assertPool(t, calculatedPools["amd"], 0, "0", "11Gi")

	departments := departmentPoolsByKey(snapshot.DepartmentPools())
	assertMemory(t, departments["risk/arm"].AllocatedLimits, "11Gi")
	assertMemory(t, departments["risk/amd"].AllocatedLimits, "11Gi")

	diagnostics := snapshot.Diagnostics()
	if diagnostics.InvalidNamespaces != 1 || diagnostics.FallbackPoolPods != 1 ||
		diagnostics.UnresolvedPods != 1 || diagnostics.DeclaredPoolMismatches != 2 {
		t.Fatalf("unexpected diagnostics: %+v", diagnostics)
	}

	returnedPools := snapshot.Pools()
	returnedPools[0].AllocatedLimits[corev1.ResourceMemory] = resource.MustParse("999Gi")
	assertMemory(t, poolsByName(snapshot.Pools())["amd"].AllocatedLimits, "11Gi")
}

func TestCalculateSelectorOverlap(t *testing.T) {
	pools := []quotav1alpha1.ResourcePool{
		resourcePool("arm", map[string]string{"arch": "arm64", "os": "kylin"}),
		resourcePool("kylin", map[string]string{"os": "kylin"}),
	}
	nodes := []corev1.Node{
		node("overlap", map[string]string{"arch": "arm64", "os": "kylin"}, "64Gi", true, false),
	}
	pods := []corev1.Pod{
		pod("system", "fallback", "overlap", "arm", "2Gi", corev1.PodRunning),
	}

	snapshot, err := Calculate(pools, nodes, nil, pods)
	if err != nil {
		t.Fatalf("Calculate() returned an error: %v", err)
	}
	calculated := poolsByName(snapshot.Pools())
	if !calculated["arm"].HasSelectorOverlap || !calculated["kylin"].HasSelectorOverlap {
		t.Fatalf("overlap was not recorded: %+v", calculated)
	}
	assertPool(t, calculated["arm"], 0, "0", "2Gi")
	assertPool(t, calculated["kylin"], 0, "0", "0")
	if diagnostics := snapshot.Diagnostics(); diagnostics.OverlappingNodes != 1 || diagnostics.FallbackPoolPods != 1 {
		t.Fatalf("unexpected diagnostics: %+v", diagnostics)
	}
}

func TestCalculateRejectsInvalidSelector(t *testing.T) {
	pools := []quotav1alpha1.ResourcePool{{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid"},
		Spec: quotav1alpha1.ResourcePoolSpec{NodeSelector: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "arch",
				Operator: metav1.LabelSelectorOperator("Invalid"),
			}},
		}},
	}}

	if _, err := Calculate(pools, nil, nil, nil); err == nil {
		t.Fatal("Calculate() should reject an invalid selector")
	}
}

func TestCalculateObservedUsage(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	pools := []quotav1alpha1.ResourcePool{
		resourcePool("arm", map[string]string{"arch": "arm64"}),
		resourcePool("amd", map[string]string{"arch": "amd64"}),
	}
	nodes := []corev1.Node{
		node("arm-node", map[string]string{"arch": "arm64"}, "8Gi", true, false),
		node("amd-node", map[string]string{"arch": "amd64"}, "8Gi", true, false),
	}
	namespaces := []corev1.Namespace{
		managedNamespace("business", "trading"),
		{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	}
	pods := []corev1.Pod{
		pod("business", "scheduled", "arm-node", "amd", "4Gi", corev1.PodRunning),
		pod("business", "pending", "", "amd", "2Gi", corev1.PodPending),
		pod("kube-system", "system", "amd-node", "amd", "1Gi", corev1.PodRunning),
		pod("business", "finished", "arm-node", "arm", "1Gi", corev1.PodSucceeded),
	}
	samples := []PodUsageSample{
		{Namespace: "business", Name: "scheduled", Timestamp: now.Add(-time.Minute), Memory: resource.MustParse("1Gi")},
		{Namespace: "business", Name: "pending", Timestamp: now, Memory: resource.MustParse("512Mi")},
		{Namespace: "kube-system", Name: "system", Timestamp: now, Memory: resource.MustParse("256Mi")},
		{Namespace: "business", Name: "finished", Timestamp: now, Memory: resource.MustParse("128Mi")},
		{Namespace: "business", Name: "missing", Timestamp: now, Memory: resource.MustParse("1Gi")},
	}

	snapshot, err := CalculateObservedUsage(pools, nodes, namespaces, pods, samples)
	if err != nil {
		t.Fatalf("CalculateObservedUsage() error = %v", err)
	}
	if snapshot.SampleCount() != 3 {
		t.Fatalf("SampleCount() = %d, want 3", snapshot.SampleCount())
	}
	if newest := snapshot.NewestSampleTime(); newest == nil || !newest.Equal(now) {
		t.Fatalf("NewestSampleTime() = %v, want %v", newest, now)
	}

	poolUsage := make(map[string]corev1.ResourceList)
	for _, usage := range snapshot.Pools() {
		poolUsage[usage.Pool] = usage.ObservedUsage
	}
	assertMemory(t, poolUsage["arm"], "1Gi")
	assertMemory(t, poolUsage["amd"], "768Mi")

	departmentUsage := snapshot.DepartmentPools()
	if len(departmentUsage) != 2 {
		t.Fatalf("DepartmentPools() length = %d, want 2", len(departmentUsage))
	}
	assertMemory(t, departmentUsage[0].ObservedUsage, "512Mi")
	assertMemory(t, departmentUsage[1].ObservedUsage, "1Gi")
	if departmentUsage[0].Department != "trading" || departmentUsage[0].Pool != "amd" ||
		departmentUsage[1].Department != "trading" || departmentUsage[1].Pool != "arm" {
		t.Fatalf("DepartmentPools() = %+v, want trading/amd then trading/arm", departmentUsage)
	}
}

func limits(memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
	}
}

func podWithResources(
	containers []corev1.ResourceRequirements,
	initContainers []corev1.ResourceRequirements,
	overhead string,
) corev1.Pod {
	pod := corev1.Pod{}
	for i := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Resources: containers[i]})
	}
	for i := range initContainers {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Resources: initContainers[i]})
	}
	if overhead != "" {
		pod.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(overhead)}
	}
	return pod
}

func resourcePool(name string, matchLabels map[string]string) quotav1alpha1.ResourcePool {
	return quotav1alpha1.ResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: quotav1alpha1.ResourcePoolSpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

func node(
	name string,
	nodeLabels map[string]string,
	memory string,
	ready bool,
	unschedulable bool,
) corev1.Node {
	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: readyStatus}},
		},
	}
}

func managedNamespace(name, department string) corev1.Namespace {
	return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			NamespaceEnforcedLabel:   "true",
			NamespaceDepartmentLabel: department,
		},
	}}
}

func pod(
	namespace string,
	name string,
	nodeName string,
	pool string,
	memory string,
	phase corev1.PodPhase,
) corev1.Pod {
	podLabels := map[string]string{}
	if pool != "" {
		podLabels[PodResourcePoolLabel] = pool
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: podLabels},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: limits(memory),
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func poolsByName(pools []Pool) map[string]Pool {
	result := make(map[string]Pool, len(pools))
	for _, pool := range pools {
		result[pool.Name] = pool
	}
	return result
}

func departmentPoolsByKey(pools []DepartmentPool) map[string]DepartmentPool {
	result := make(map[string]DepartmentPool, len(pools))
	for _, pool := range pools {
		result[pool.Department+"/"+pool.Pool] = pool
	}
	return result
}

func assertPool(t *testing.T, pool Pool, readyNodes int32, allocatable, allocated string) {
	t.Helper()
	if pool.ReadyNodes != readyNodes {
		t.Fatalf("pool %q ready nodes = %d, want %d", pool.Name, pool.ReadyNodes, readyNodes)
	}
	assertMemory(t, pool.Allocatable, allocatable)
	assertMemory(t, pool.AllocatedLimits, allocated)
}

func assertMemory(t *testing.T, resources corev1.ResourceList, want string) {
	t.Helper()
	wantResources := corev1.ResourceList{}
	if want != "0" {
		wantResources[corev1.ResourceMemory] = resource.MustParse(want)
	}
	if !quota.Equal(resources, wantResources) {
		t.Fatalf("memory resources = %v, want %v", resources, wantResources)
	}
}
