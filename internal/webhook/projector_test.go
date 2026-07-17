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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

func TestBuiltinProjectorProjectsWorkloads(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kubernetes scheme: %v", err)
	}
	decoder, err := admission.NewDecoder(scheme)
	if err != nil {
		t.Fatalf("create decoder: %v", err)
	}
	projector := NewBuiltinProjector(decoder, fake.NewClientBuilder().WithScheme(scheme).Build())
	replicas := int32(3)
	parallelism := int32(2)
	tests := []struct {
		name              string
		kind              schema.GroupVersionKind
		object            runtime.Object
		expectedMemory    string
		expectedProjected bool
	}{
		{
			name:              "pod",
			kind:              corev1.SchemeGroupVersion.WithKind("Pod"),
			object:            testAdmissionPod("2Gi"),
			expectedMemory:    "2Gi",
			expectedProjected: true,
		},
		{
			name: "deployment replicas",
			kind: appsv1.SchemeGroupVersion.WithKind("Deployment"),
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: testPodTemplate("2Gi"),
			}},
			expectedMemory:    "6Gi",
			expectedProjected: true,
		},
		{
			name: "statefulset replicas",
			kind: appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
			object: &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{
				Replicas: &replicas,
				Template: testPodTemplate("2Gi"),
			}},
			expectedMemory:    "6Gi",
			expectedProjected: true,
		},
		{
			name: "replicaset replicas",
			kind: appsv1.SchemeGroupVersion.WithKind("ReplicaSet"),
			object: &appsv1.ReplicaSet{Spec: appsv1.ReplicaSetSpec{
				Replicas: &replicas,
				Template: testPodTemplate("2Gi"),
			}},
			expectedMemory:    "6Gi",
			expectedProjected: true,
		},
		{
			name: "job parallelism",
			kind: batchv1.SchemeGroupVersion.WithKind("Job"),
			object: &batchv1.Job{Spec: batchv1.JobSpec{
				Parallelism: &parallelism,
				Template:    testPodTemplate("2Gi"),
			}},
			expectedMemory:    "4Gi",
			expectedProjected: true,
		},
		{
			name: "forbid-concurrent cronjob projects one job",
			kind: batchv1.SchemeGroupVersion.WithKind("CronJob"),
			object: &batchv1.CronJob{Spec: batchv1.CronJobSpec{
				ConcurrencyPolicy: batchv1.ForbidConcurrent,
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
					Parallelism: &parallelism,
					Template:    testPodTemplate("2Gi"),
				}},
			}},
			expectedMemory:    "4Gi",
			expectedProjected: true,
		},
		{
			name: "concurrent cronjob defers resource estimate to pods",
			kind: batchv1.SchemeGroupVersion.WithKind("CronJob"),
			object: &batchv1.CronJob{Spec: batchv1.CronJobSpec{
				ConcurrencyPolicy: batchv1.AllowConcurrent,
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
					Parallelism: &parallelism,
					Template:    testPodTemplate("2Gi"),
				}},
			}},
			expectedMemory:    "0",
			expectedProjected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection, err := projector.Project(context.Background(), test.kind, rawObject(t, test.object))
			if err != nil {
				t.Fatalf("Project() returned an error: %v", err)
			}
			assertProjectedMemory(t, projection.Limits, test.expectedMemory)
			if projection.HasResourceProjection != test.expectedProjected {
				t.Fatalf("HasResourceProjection = %t, want %t", projection.HasResourceProjection, test.expectedProjected)
			}
			if projection.Department != "risk" || projection.Pool != "amd" {
				t.Fatalf("projection identity = %s/%s, want risk/amd", projection.Department, projection.Pool)
			}
		})
	}
}

func TestBuiltinProjectorCountsDaemonSetTargetNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kubernetes scheme: %v", err)
	}
	decoder, err := admission.NewDecoder(scheme)
	if err != nil {
		t.Fatalf("create decoder: %v", err)
	}
	nodes := []runtime.Object{
		testProjectionNode("amd-ready", "amd64", true, false),
		testProjectionNode("amd-cordoned", "amd64", true, true),
		testProjectionNode("arm-ready", "arm64", true, false),
	}
	projector := NewBuiltinProjector(
		decoder,
		fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build(),
	)
	daemonSet := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: testPodTemplate("2Gi")}}
	projection, err := projector.Project(
		context.Background(),
		appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
		rawObject(t, daemonSet),
	)
	if err != nil {
		t.Fatalf("Project() returned an error: %v", err)
	}
	assertProjectedMemory(t, projection.Limits, "2Gi")
}

func testAdmissionPod(memory string) *corev1.Pod {
	template := testPodTemplate(memory)
	return &corev1.Pod{ObjectMeta: template.ObjectMeta, Spec: template.Spec}
}

func testPodTemplate(memory string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			PodDepartmentLabel:              "risk",
			accounting.PodResourcePoolLabel: "amd",
		}},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"arch": "amd64"},
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse(memory),
				}},
			}},
		},
	}
}

func testProjectionNode(name, architecture string, ready, cordoned bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"arch": architecture}},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: status,
		}}},
	}
}

func rawObject(t *testing.T, object runtime.Object) runtime.RawExtension {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	return runtime.RawExtension{Raw: raw}
}

func assertProjectedMemory(t *testing.T, resources corev1.ResourceList, expected string) {
	t.Helper()
	memory := resources[corev1.ResourceMemory]
	if expected == "0" {
		if memory.Sign() != 0 {
			t.Fatalf("projected memory = %s, want 0", memory.String())
		}
		return
	}
	want := resource.MustParse(expected)
	if memory.Cmp(want) != 0 {
		t.Fatalf("projected memory = %s, want %s", memory.String(), expected)
	}
}
