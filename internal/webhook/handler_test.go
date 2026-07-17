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

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

func TestHandlerAdmissionModes(t *testing.T) {
	tests := []struct {
		name            string
		mode            quotav1alpha1.AdmissionMode
		memory          string
		expectedAllowed bool
		expectedWarning bool
	}{
		{name: "enforce allows request within quota", mode: quotav1alpha1.AdmissionModeEnforce, memory: "4Gi", expectedAllowed: true},
		{name: "enforce denies request over quota", mode: quotav1alpha1.AdmissionModeEnforce, memory: "6Gi", expectedAllowed: false},
		{name: "warn allows with warning", mode: quotav1alpha1.AdmissionModeWarn, memory: "6Gi", expectedAllowed: true, expectedWarning: true},
		{name: "observe allows without warning", mode: quotav1alpha1.AdmissionModeObserve, memory: "6Gi", expectedAllowed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, now := testHandler(t, test.mode)
			handler.now = func() time.Time { return now }
			response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, testAdmissionPod(test.memory), nil))
			if response.Allowed != test.expectedAllowed {
				t.Fatalf("Allowed = %t, want %t; result=%+v", response.Allowed, test.expectedAllowed, response.Result)
			}
			if (len(response.Warnings) > 0) != test.expectedWarning {
				t.Fatalf("warnings = %v, expectedWarning=%t", response.Warnings, test.expectedWarning)
			}
			if !response.Allowed && (response.Result == nil || !strings.Contains(response.Result.Message, "allocatedLimits=5Gi")) {
				t.Fatalf("denial message = %v, want allocated quota details", response.Result)
			}
		})
	}
}

func TestHandlerUpdateOnlyChecksPositiveMemoryDelta(t *testing.T) {
	tests := []struct {
		name string
		old  *corev1.Pod
		new  *corev1.Pod
	}{
		{name: "memory limit decrease", old: testAdmissionPod("8Gi"), new: testAdmissionPod("4Gi")},
		{name: "memory limit unchanged", old: testAdmissionPod("4Gi"), new: testAdmissionPod("4Gi")},
		{name: "request and cpu changes are ignored", old: testAdmissionPod("4Gi"), new: testAdmissionPod("4Gi")},
	}
	tests[2].new.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("3Gi"),
		corev1.ResourceCPU:    resource.MustParse("2"),
	}
	tests[2].new.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("4")

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
			handler.now = func() time.Time { return now.Add(24 * time.Hour) }
			response := handler.Handle(context.Background(), podRequest(t, admissionv1.Update, test.new, test.old))
			if !response.Allowed {
				t.Fatalf("update was denied: %+v", response.Result)
			}
		})
	}
}

func TestHandlerDeniesPositiveUpdateDelta(t *testing.T) {
	handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
	handler.now = func() time.Time { return now }
	response := handler.Handle(
		context.Background(),
		podRequest(t, admissionv1.Update, testAdmissionPod("10Gi"), testAdmissionPod("4Gi")),
	)
	if response.Allowed {
		t.Fatal("positive UPDATE delta was allowed, want denial")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "limitDelta=6Gi") {
		t.Fatalf("message = %v, want positive delta details", response.Result)
	}
}

func TestHandlerDeniesRemovingMemoryLimit(t *testing.T) {
	handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
	handler.now = func() time.Time { return now }
	oldPod := testAdmissionPod("4Gi")
	newPod := testAdmissionPod("4Gi")
	newPod.Spec.Containers[0].Resources.Limits = nil
	response := handler.Handle(context.Background(), podRequest(t, admissionv1.Update, newPod, oldPod))
	if response.Allowed {
		t.Fatal("removing limits.memory was allowed")
	}
}

func TestHandlerChecksDeploymentProjection(t *testing.T) {
	handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
	handler.now = func() time.Time { return now }
	replicas := int32(3)
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Replicas: &replicas,
		Template: testPodTemplate("2Gi"),
	}}
	response := handler.Handle(
		context.Background(),
		objectRequest(t, admissionv1.Create, "risk-apps", appsv1.SchemeGroupVersion.WithKind("Deployment"), deployment, nil),
	)
	if response.Allowed {
		t.Fatal("Deployment projected over quota was allowed")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, "limitDelta=6Gi") {
		t.Fatalf("message = %v, want Deployment projection details", response.Result)
	}
}

func TestHandlerChecksMaxQuotaAndPoolAdmissionBudget(t *testing.T) {
	t.Run("department max quota", func(t *testing.T) {
		handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
		handler.now = func() time.Time { return now }
		quotaObject := &quotav1alpha1.DepartmentQuota{}
		if err := handler.reader.Get(context.Background(), types.NamespacedName{Name: "risk"}, quotaObject); err != nil {
			t.Fatalf("get department quota: %v", err)
		}
		quotaObject.Status.Pools[0].EffectiveQuota = memoryResources("30Gi")
		if err := handlerClient(t, handler).Update(context.Background(), quotaObject); err != nil {
			t.Fatalf("update department quota: %v", err)
		}
		response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, testAdmissionPod("11Gi"), nil))
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "maxQuota=15Gi") {
			t.Fatalf("max quota response = allowed:%t result:%+v", response.Allowed, response.Result)
		}
	})

	t.Run("resource pool admission budget", func(t *testing.T) {
		handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
		handler.now = func() time.Time { return now }
		quotaObject := &quotav1alpha1.DepartmentQuota{}
		if err := handler.reader.Get(context.Background(), types.NamespacedName{Name: "risk"}, quotaObject); err != nil {
			t.Fatalf("get department quota: %v", err)
		}
		quotaObject.Spec.Pools[0].MaxQuota = memoryResources("40Gi")
		quotaObject.Status.Pools[0].EffectiveQuota = memoryResources("30Gi")
		if err := handlerClient(t, handler).Update(context.Background(), quotaObject); err != nil {
			t.Fatalf("update department quota: %v", err)
		}
		pool := &quotav1alpha1.ResourcePool{}
		if err := handler.reader.Get(context.Background(), types.NamespacedName{Name: "amd"}, pool); err != nil {
			t.Fatalf("get resource pool: %v", err)
		}
		pool.Status.AllocatedLimits = memoryResources("19Gi")
		if err := handlerClient(t, handler).Update(context.Background(), pool); err != nil {
			t.Fatalf("update resource pool: %v", err)
		}
		response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, testAdmissionPod("2Gi"), nil))
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "admissionBudget=20Gi") {
			t.Fatalf("admission budget response = allowed:%t result:%+v", response.Allowed, response.Result)
		}
	})
}

func TestHandlerSnapshotAndUnknownPoolPolicies(t *testing.T) {
	t.Run("stale snapshot fails closed", func(t *testing.T) {
		handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
		handler.now = func() time.Time { return now.Add(2 * time.Minute) }
		response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, testAdmissionPod("1Gi"), nil))
		if response.Allowed {
			t.Fatal("stale snapshot was allowed with fail-closed enabled")
		}
		if response.Result == nil || !strings.Contains(response.Result.Message, "snapshot is stale") {
			t.Fatalf("message = %v, want stale snapshot details", response.Result)
		}
	})

	t.Run("stale snapshot may fail open", func(t *testing.T) {
		handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
		policy := getPolicy(t, handler)
		policy.Spec.Admission.FailClosedOnStaleSnapshot = false
		if err := handlerClient(t, handler).Update(context.Background(), policy); err != nil {
			t.Fatalf("update policy: %v", err)
		}
		handler.now = func() time.Time { return now.Add(2 * time.Minute) }
		response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, testAdmissionPod("1Gi"), nil))
		if !response.Allowed || len(response.Warnings) == 0 {
			t.Fatalf("stale fail-open response = allowed:%t warnings:%v", response.Allowed, response.Warnings)
		}
	})

	t.Run("unknown pool follows fail-closed switch", func(t *testing.T) {
		handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
		handler.now = func() time.Time { return now }
		pod := testAdmissionPod("1Gi")
		pod.Labels[accounting.PodResourcePoolLabel] = "unknown"
		response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, pod, nil))
		if response.Allowed {
			t.Fatal("unknown pool was allowed with fail-closed enabled")
		}

		policy := getPolicy(t, handler)
		policy.Spec.Admission.FailClosedOnUnknownPool = false
		if err := handlerClient(t, handler).Update(context.Background(), policy); err != nil {
			t.Fatalf("update policy: %v", err)
		}
		response = handler.Handle(context.Background(), podRequest(t, admissionv1.Create, pod, nil))
		if !response.Allowed || len(response.Warnings) == 0 {
			t.Fatalf("unknown-pool fail-open response = allowed:%t warnings:%v", response.Allowed, response.Warnings)
		}
	})
}

func TestHandlerValidatesIdentityAndHardPoolConstraint(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*corev1.Pod)
		wantPhrase string
	}{
		{
			name: "missing memory limit",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Resources.Limits = nil
			},
			wantPhrase: "must declare a positive limits.memory",
		},
		{
			name: "missing department",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, PodDepartmentLabel)
			},
			wantPhrase: "requires Pod template label department=risk",
		},
		{
			name: "department mismatch",
			mutate: func(pod *corev1.Pod) {
				pod.Labels[PodDepartmentLabel] = "trading"
			},
			wantPhrase: "does not match Namespace department risk",
		},
		{
			name: "missing pool",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, accounting.PodResourcePoolLabel)
			},
			wantPhrase: "requires Pod template label quota.kgpp34.io/resource-pool",
		},
		{
			name: "preferred or absent affinity cannot prove pool",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.NodeSelector = nil
			},
			wantPhrase: "hard node constraints do not guarantee",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, now := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
			handler.now = func() time.Time { return now }
			pod := testAdmissionPod("1Gi")
			test.mutate(pod)
			response := handler.Handle(context.Background(), podRequest(t, admissionv1.Create, pod, nil))
			if response.Allowed {
				t.Fatalf("invalid object was allowed")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, test.wantPhrase) {
				t.Fatalf("message = %v, want phrase %q", response.Result, test.wantPhrase)
			}
		})
	}
}

func TestHandlerAlwaysAllowsDeleteAndUnmanagedNamespace(t *testing.T) {
	handler, _ := testHandler(t, quotav1alpha1.AdmissionModeEnforce)
	deleteResponse := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
	}})
	if !deleteResponse.Allowed {
		t.Fatalf("DELETE was denied: %+v", deleteResponse.Result)
	}

	request := podRequest(t, admissionv1.Create, testAdmissionPod("100Gi"), nil)
	request.Namespace = "system"
	unmanagedResponse := handler.Handle(context.Background(), request)
	if !unmanagedResponse.Allowed {
		t.Fatalf("unmanaged namespace request was denied: %+v", unmanagedResponse.Result)
	}
}

func testHandler(t *testing.T, mode quotav1alpha1.AdmissionMode) (*Handler, time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kubernetes scheme: %v", err)
	}
	if err := quotav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add quota scheme: %v", err)
	}
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "risk-apps",
		Labels: map[string]string{
			accounting.NamespaceEnforcedLabel:   "true",
			accounting.NamespaceDepartmentLabel: "risk",
		},
	}}
	systemNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}}
	policy := &quotav1alpha1.ElasticQuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: quotav1alpha1.ElasticQuotaPolicySpec{Admission: quotav1alpha1.AdmissionPolicy{
			Mode:                      mode,
			MaxSnapshotAge:            metav1.Duration{Duration: time.Minute},
			FailClosedOnStaleSnapshot: true,
			FailClosedOnUnknownPool:   true,
		}},
		Status: quotav1alpha1.ElasticQuotaPolicyStatus{LastCalculationTime: &metav1.Time{Time: now}},
	}
	pool := &quotav1alpha1.ResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "amd"},
		Spec: quotav1alpha1.ResourcePoolSpec{NodeSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{"arch": "amd64"},
		}},
		Status: quotav1alpha1.ResourcePoolStatus{
			AllocatedLimits: memoryResources("5Gi"),
			AdmissionBudget: memoryResources("20Gi"),
		},
	}
	departmentQuota := &quotav1alpha1.DepartmentQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "risk"},
		Spec: quotav1alpha1.DepartmentQuotaSpec{
			Department: "risk",
			Pools: []quotav1alpha1.DepartmentPoolQuota{{
				Name: "amd", BaseQuota: memoryResources("1Gi"), MaxQuota: memoryResources("15Gi"),
			}},
		},
		Status: quotav1alpha1.DepartmentQuotaStatus{Pools: []quotav1alpha1.DepartmentPoolQuotaStatus{{
			Name: "amd", AllocatedLimits: memoryResources("5Gi"), EffectiveQuota: memoryResources("10Gi"),
		}}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespace,
		systemNamespace,
		policy,
		pool,
		departmentQuota,
	).Build()
	decoder, err := admission.NewDecoder(scheme)
	if err != nil {
		t.Fatalf("create admission decoder: %v", err)
	}
	projector := NewBuiltinProjector(decoder, reader)
	return NewHandler(reader, projector, logr.Discard()), now
}

func podRequest(
	t *testing.T,
	operation admissionv1.Operation,
	object *corev1.Pod,
	oldObject *corev1.Pod,
) admission.Request {
	t.Helper()
	return objectRequest(t, operation, "risk-apps", corev1.SchemeGroupVersion.WithKind("Pod"), object, oldObject)
}

func objectRequest(
	t *testing.T,
	operation admissionv1.Operation,
	namespace string,
	kind schema.GroupVersionKind,
	object runtime.Object,
	oldObject runtime.Object,
) admission.Request {
	t.Helper()
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Namespace: namespace,
		Name:      "app",
		Kind: metav1.GroupVersionKind{
			Group: kind.Group, Version: kind.Version, Kind: kind.Kind,
		},
		Object: rawObject(t, object),
	}}
	if oldObject != nil {
		request.OldObject = rawObject(t, oldObject)
	}
	return request
}

func memoryResources(value string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(value)}
}

func getPolicy(t *testing.T, handler *Handler) *quotav1alpha1.ElasticQuotaPolicy {
	t.Helper()
	policy := &quotav1alpha1.ElasticQuotaPolicy{}
	if err := handler.reader.Get(context.Background(), types.NamespacedName{Name: "default"}, policy); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	return policy
}

func handlerClient(t *testing.T, handler *Handler) client.Client {
	t.Helper()
	kubeClient, ok := handler.reader.(client.Client)
	if !ok {
		t.Fatal("test handler reader does not implement client.Client")
	}
	return kubeClient
}
