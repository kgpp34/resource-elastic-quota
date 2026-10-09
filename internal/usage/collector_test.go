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

package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgotesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

func TestMetricsAPICollectorSumsContainerMemory(t *testing.T) {
	podMetrics := metricsv1beta1.PodMetrics{
		TypeMeta:   metav1.TypeMeta{APIVersion: "metrics.k8s.io/v1beta1", Kind: "PodMetrics"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "business", Name: "api"},
		Timestamp:  metav1.NewTime(time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)),
		Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
			{Name: "sidecar", Usage: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}},
		},
	}
	client := metricsfake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{podMetrics}}, nil
	})
	samples, err := NewMetricsAPICollector(client).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(samples) != 1 || samples[0].Memory.Cmp(resource.MustParse("384Mi")) != 0 {
		t.Fatalf("Collect() = %+v, want one 384Mi sample", samples)
	}
}

func TestCacheUsesLastGoodSamplesAfterRefreshFailure(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	collector := &fakeCollector{samples: []accounting.PodUsageSample{{
		Namespace: "business", Name: "api", Timestamp: now, Memory: resource.MustParse("1Gi"),
	}}}
	cache := NewCache(collector)
	first, collected, err := cache.Get(context.Background(), now, time.Minute)
	if err != nil || len(first) != 1 || !collected.Equal(now) {
		t.Fatalf("first Get() = (%+v, %v, %v)", first, collected, err)
	}
	first[0].Memory.Set(0)
	collector.err = errors.New("metrics unavailable")
	second, secondCollected, err := cache.Get(context.Background(), now.Add(2*time.Minute), time.Minute)
	if err == nil || len(second) != 1 || second[0].Memory.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("second Get() = (%+v, %v, %v), want cached 1Gi plus error", second, secondCollected, err)
	}
}

type fakeCollector struct {
	samples []accounting.PodUsageSample
	err     error
}

func (f *fakeCollector) Collect(context.Context) ([]accounting.PodUsageSample, error) {
	return f.samples, f.err
}
