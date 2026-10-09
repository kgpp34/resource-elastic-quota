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

// Package usage collects actual Pod memory usage without making it part of
// admission decisions. Limit accounting remains the quota enforcement source.
package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
)

const DefaultCollectionInterval = 30 * time.Second

// Collector returns one point-in-time sample per Pod.
type Collector interface {
	Collect(context.Context) ([]accounting.PodUsageSample, error)
}

// MetricsAPICollector reads metrics.k8s.io, normally served by metrics-server.
type MetricsAPICollector struct {
	client metricsclient.Interface
}

func NewMetricsAPICollector(client metricsclient.Interface) *MetricsAPICollector {
	return &MetricsAPICollector{client: client}
}

func (c *MetricsAPICollector) Collect(ctx context.Context) ([]accounting.PodUsageSample, error) {
	metrics, err := c.client.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pod metrics: %w", err)
	}
	samples := make([]accounting.PodUsageSample, 0, len(metrics.Items))
	for i := range metrics.Items {
		podMetrics := metrics.Items[i]
		memory := corev1.ResourceList{}
		for j := range podMetrics.Containers {
			if quantity, exists := podMetrics.Containers[j].Usage[corev1.ResourceMemory]; exists {
				memory = addMemory(memory, quantity)
			}
		}
		quantity, exists := memory[corev1.ResourceMemory]
		if !exists {
			continue
		}
		samples = append(samples, accounting.PodUsageSample{
			Namespace: podMetrics.Namespace,
			Name:      podMetrics.Name,
			Timestamp: podMetrics.Timestamp.Time,
			Memory:    quantity.DeepCopy(),
		})
	}
	return samples, nil
}

func addMemory(resources corev1.ResourceList, quantity resource.Quantity) corev1.ResourceList {
	current := resources[corev1.ResourceMemory]
	current.Add(quantity)
	resources[corev1.ResourceMemory] = current
	return resources
}

// Cache rate-limits synchronous Metrics API collection. It intentionally owns
// no goroutine: reconcile cancellation and controller-runtime lifecycle remain
// authoritative.
type Cache struct {
	collector Collector
	mu        sync.Mutex
	samples   []accounting.PodUsageSample
	collected time.Time
}

func NewCache(collector Collector) *Cache {
	return &Cache{collector: collector}
}

// Get returns a defensive copy. On refresh failure, the last successful sample
// is returned together with the error so observability can degrade gracefully.
func (c *Cache) Get(
	ctx context.Context,
	now time.Time,
	interval time.Duration,
) ([]accounting.PodUsageSample, time.Time, error) {
	if interval <= 0 {
		interval = DefaultCollectionInterval
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.collected.IsZero() && now.Sub(c.collected) < interval {
		return cloneSamples(c.samples), c.collected, nil
	}
	samples, err := c.collector.Collect(ctx)
	if err != nil {
		return cloneSamples(c.samples), c.collected, err
	}
	c.samples = cloneSamples(samples)
	c.collected = now
	return cloneSamples(c.samples), c.collected, nil
}

func cloneSamples(samples []accounting.PodUsageSample) []accounting.PodUsageSample {
	result := make([]accounting.PodUsageSample, len(samples))
	for i := range samples {
		result[i] = samples[i]
		result[i].Memory = samples[i].Memory.DeepCopy()
	}
	return result
}
