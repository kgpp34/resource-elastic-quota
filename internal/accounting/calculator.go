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

// Package accounting calculates resource pool capacity and department usage
// from an in-memory Kubernetes object snapshot.
package accounting

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/quota"
)

const (
	// NamespaceEnforcedLabel explicitly opts a namespace into department quota.
	NamespaceEnforcedLabel = "quota.kgpp34.io/enforced"
	// NamespaceDepartmentLabel is the trusted department identity set by the platform.
	NamespaceDepartmentLabel = "quota.kgpp34.io/department"
	// PodResourcePoolLabel declares the resource pool for an unscheduled Pod.
	PodResourcePoolLabel = "quota.kgpp34.io/resource-pool"
)

// Pool contains the calculated state for one ResourcePool.
type Pool struct {
	Name               string
	ReadyNodes         int32
	Allocatable        corev1.ResourceList
	AllocatedLimits    corev1.ResourceList
	HasSelectorOverlap bool
	HasMemoryPressure  bool
}

// DepartmentPool contains one department's aggregate limits in one pool.
type DepartmentPool struct {
	Department      string
	Pool            string
	AllocatedLimits corev1.ResourceList
}

// PodUsageSample contains one metrics-server memory sample. Namespace and Name
// identify the Pod whose placement and trusted department are resolved from the
// Kubernetes object snapshot rather than from metrics labels.
type PodUsageSample struct {
	Namespace string
	Name      string
	Timestamp time.Time
	Memory    resource.Quantity
}

// PoolUsage contains observed memory usage for all sampled Pods in one pool.
type PoolUsage struct {
	Pool          string
	ObservedUsage corev1.ResourceList
}

// DepartmentPoolUsage contains observed memory usage for one department and pool.
type DepartmentPoolUsage struct {
	Department    string
	Pool          string
	ObservedUsage corev1.ResourceList
}

// UsageSnapshot is an immutable-by-convention projection of metrics samples.
type UsageSnapshot struct {
	pools       []PoolUsage
	departments []DepartmentPoolUsage
	newest      *time.Time
	sampleCount int
}

// Pools returns a deep copy sorted by pool name.
func (s UsageSnapshot) Pools() []PoolUsage {
	result := make([]PoolUsage, len(s.pools))
	for i := range s.pools {
		result[i] = PoolUsage{
			Pool:          s.pools[i].Pool,
			ObservedUsage: quota.Clone(s.pools[i].ObservedUsage),
		}
	}
	return result
}

// DepartmentPools returns a deep copy sorted by department and pool name.
func (s UsageSnapshot) DepartmentPools() []DepartmentPoolUsage {
	result := make([]DepartmentPoolUsage, len(s.departments))
	for i := range s.departments {
		result[i] = DepartmentPoolUsage{
			Department:    s.departments[i].Department,
			Pool:          s.departments[i].Pool,
			ObservedUsage: quota.Clone(s.departments[i].ObservedUsage),
		}
	}
	return result
}

// NewestSampleTime returns the newest included metrics timestamp.
func (s UsageSnapshot) NewestSampleTime() *time.Time {
	if s.newest == nil {
		return nil
	}
	result := *s.newest
	return &result
}

// SampleCount returns the number of Pod samples included in the snapshot.
func (s UsageSnapshot) SampleCount() int {
	return s.sampleCount
}

// Diagnostics records data that could not be classified without weakening
// the accounting result.
type Diagnostics struct {
	UnmatchedNodes         int
	OverlappingNodes       int
	InvalidNamespaces      int
	FallbackPoolPods       int
	UnresolvedPods         int
	DeclaredPoolMismatches int
}

// IsDegraded reports whether some Pod or Node data was ambiguous.
func (d Diagnostics) IsDegraded() bool {
	return d.OverlappingNodes > 0 || d.InvalidNamespaces > 0 ||
		d.FallbackPoolPods > 0 || d.UnresolvedPods > 0 ||
		d.DeclaredPoolMismatches > 0
}

// Snapshot is an immutable-by-convention result. Accessors return deep copies
// so later admission code cannot mutate controller accounting state.
type Snapshot struct {
	pools       []Pool
	departments []DepartmentPool
	diagnostics Diagnostics
}

// Pools returns a deep copy sorted by ResourcePool name.
func (s Snapshot) Pools() []Pool {
	result := make([]Pool, len(s.pools))
	for i := range s.pools {
		result[i] = clonePool(s.pools[i])
	}
	return result
}

// DepartmentPools returns a deep copy sorted by department and pool name.
func (s Snapshot) DepartmentPools() []DepartmentPool {
	result := make([]DepartmentPool, len(s.departments))
	for i := range s.departments {
		result[i] = DepartmentPool{
			Department:      s.departments[i].Department,
			Pool:            s.departments[i].Pool,
			AllocatedLimits: quota.Clone(s.departments[i].AllocatedLimits),
		}
	}
	return result
}

// Diagnostics returns classification counters for this calculation.
func (s Snapshot) Diagnostics() Diagnostics {
	return s.diagnostics
}

// PodLimits calculates the Kubernetes Pod-level limits for memory only:
// max(sum(regular containers), max(init containers)) + Pod overhead.
func PodLimits(pod corev1.Pod) corev1.ResourceList {
	regular := resource.Quantity{}
	initMaximum := resource.Quantity{}
	hasMemory := false

	for i := range pod.Spec.Containers {
		memory, ok := pod.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory]
		if !ok {
			continue
		}
		hasMemory = true
		regular.Add(memory)
	}

	for i := range pod.Spec.InitContainers {
		memory, ok := pod.Spec.InitContainers[i].Resources.Limits[corev1.ResourceMemory]
		if !ok {
			continue
		}
		hasMemory = true
		if memory.Cmp(initMaximum) > 0 {
			initMaximum = memory.DeepCopy()
		}
	}

	effective := regular
	if initMaximum.Cmp(effective) > 0 {
		effective = initMaximum
	}

	if overhead, ok := pod.Spec.Overhead[corev1.ResourceMemory]; ok {
		hasMemory = true
		effective.Add(overhead)
	}

	if !hasMemory {
		return corev1.ResourceList{}
	}
	return corev1.ResourceList{corev1.ResourceMemory: effective}
}

// ShouldCountPod reports whether a Pod still reserves quota.
func ShouldCountPod(pod corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// Calculate rebuilds all capacity and limit accounting from Kubernetes objects.
func Calculate(
	resourcePools []quotav1alpha1.ResourcePool,
	nodes []corev1.Node,
	namespaces []corev1.Namespace,
	pods []corev1.Pod,
) (Snapshot, error) {
	selectors, err := compilePoolSelectors(resourcePools)
	if err != nil {
		return Snapshot{}, err
	}

	classification := classifyNodes(selectors, nodes)
	managedNamespaces, invalidNamespaces := classifyNamespaces(namespaces)
	classification.diagnostics.InvalidNamespaces = invalidNamespaces

	departmentLimits := make(map[departmentPoolKey]corev1.ResourceList)
	for i := range pods {
		pod := pods[i]
		if !ShouldCountPod(pod) {
			continue
		}

		department, managed := managedNamespaces[pod.Namespace]
		poolName, resolved := resolvePodPool(pod, &classification)
		if !resolved {
			if managed {
				classification.diagnostics.UnresolvedPods++
			}
			continue
		}

		limits := PodLimits(pod)
		pool := classification.pools[poolName]
		pool.AllocatedLimits = quota.Add(pool.AllocatedLimits, limits)
		classification.pools[poolName] = pool

		if !managed {
			continue
		}
		key := departmentPoolKey{department: department, pool: poolName}
		departmentLimits[key] = quota.Add(departmentLimits[key], limits)
	}

	return newSnapshot(classification, departmentLimits), nil
}

// CalculateObservedUsage attributes metrics-server samples using the same
// trusted Namespace and actual-node rules as strict limits accounting. Samples
// without a live, non-terminal Pod or a resolvable pool are ignored.
func CalculateObservedUsage(
	resourcePools []quotav1alpha1.ResourcePool,
	nodes []corev1.Node,
	namespaces []corev1.Namespace,
	pods []corev1.Pod,
	samples []PodUsageSample,
) (UsageSnapshot, error) {
	selectors, err := compilePoolSelectors(resourcePools)
	if err != nil {
		return UsageSnapshot{}, err
	}
	classification := classifyNodes(selectors, nodes)
	managedNamespaces, _ := classifyNamespaces(namespaces)
	podsByKey := make(map[string]corev1.Pod, len(pods))
	for i := range pods {
		podsByKey[podKey(pods[i].Namespace, pods[i].Name)] = pods[i]
	}

	poolUsage := make(map[string]corev1.ResourceList)
	departmentUsage := make(map[departmentPoolKey]corev1.ResourceList)
	var newest *time.Time
	included := 0
	for i := range samples {
		sample := samples[i]
		pod, exists := podsByKey[podKey(sample.Namespace, sample.Name)]
		if !exists || !ShouldCountPod(pod) || sample.Memory.Sign() < 0 {
			continue
		}
		poolName, resolved := resolvePodPool(pod, &classification)
		if !resolved {
			continue
		}
		resources := corev1.ResourceList{corev1.ResourceMemory: sample.Memory.DeepCopy()}
		poolUsage[poolName] = quota.Add(poolUsage[poolName], resources)
		if department, managed := managedNamespaces[pod.Namespace]; managed {
			key := departmentPoolKey{department: department, pool: poolName}
			departmentUsage[key] = quota.Add(departmentUsage[key], resources)
		}
		if newest == nil || sample.Timestamp.After(*newest) {
			timestamp := sample.Timestamp
			newest = &timestamp
		}
		included++
	}

	pools := make([]PoolUsage, 0, len(poolUsage))
	for poolName, resources := range poolUsage {
		pools = append(pools, PoolUsage{Pool: poolName, ObservedUsage: quota.Clone(resources)})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Pool < pools[j].Pool })

	departments := make([]DepartmentPoolUsage, 0, len(departmentUsage))
	for key, resources := range departmentUsage {
		departments = append(departments, DepartmentPoolUsage{
			Department:    key.department,
			Pool:          key.pool,
			ObservedUsage: quota.Clone(resources),
		})
	}
	sort.Slice(departments, func(i, j int) bool {
		if departments[i].Department == departments[j].Department {
			return departments[i].Pool < departments[j].Pool
		}
		return departments[i].Department < departments[j].Department
	})
	return UsageSnapshot{
		pools:       pools,
		departments: departments,
		newest:      newest,
		sampleCount: included,
	}, nil
}

func podKey(namespace, name string) string {
	return namespace + "\x00" + name
}

type poolSelector struct {
	name     string
	selector labels.Selector
}

func compilePoolSelectors(resourcePools []quotav1alpha1.ResourcePool) ([]poolSelector, error) {
	selectors := make([]poolSelector, 0, len(resourcePools))
	for i := range resourcePools {
		selector, err := metav1.LabelSelectorAsSelector(&resourcePools[i].Spec.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("compile resource pool %q node selector: %w", resourcePools[i].Name, err)
		}
		selectors = append(selectors, poolSelector{name: resourcePools[i].Name, selector: selector})
	}
	sort.Slice(selectors, func(i, j int) bool { return selectors[i].name < selectors[j].name })
	return selectors, nil
}

type nodeClassification struct {
	poolByNode  map[string]string
	pools       map[string]Pool
	diagnostics Diagnostics
}

func classifyNodes(selectors []poolSelector, nodes []corev1.Node) nodeClassification {
	result := nodeClassification{
		poolByNode: make(map[string]string),
		pools:      make(map[string]Pool, len(selectors)),
	}
	for i := range selectors {
		result.pools[selectors[i].name] = Pool{
			Name:            selectors[i].name,
			Allocatable:     corev1.ResourceList{},
			AllocatedLimits: corev1.ResourceList{},
		}
	}

	for i := range nodes {
		node := nodes[i]
		matches := matchingPools(selectors, labels.Set(node.Labels))
		switch len(matches) {
		case 0:
			result.diagnostics.UnmatchedNodes++
		case 1:
			poolName := matches[0]
			result.poolByNode[node.Name] = poolName
			if hasMemoryPressure(node) {
				pool := result.pools[poolName]
				pool.HasMemoryPressure = true
				result.pools[poolName] = pool
			}
			if !isReadyAndSchedulable(node) {
				continue
			}
			pool := result.pools[poolName]
			if memory, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
				pool.Allocatable = quota.Add(pool.Allocatable, corev1.ResourceList{
					corev1.ResourceMemory: memory,
				})
			}
			pool.ReadyNodes++
			result.pools[poolName] = pool
		default:
			result.diagnostics.OverlappingNodes++
			for _, poolName := range matches {
				pool := result.pools[poolName]
				pool.HasSelectorOverlap = true
				result.pools[poolName] = pool
			}
		}
	}
	return result
}

func hasMemoryPressure(node corev1.Node) bool {
	for i := range node.Status.Conditions {
		condition := node.Status.Conditions[i]
		if condition.Type == corev1.NodeMemoryPressure {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func matchingPools(selectors []poolSelector, nodeLabels labels.Set) []string {
	var matches []string
	for i := range selectors {
		if selectors[i].selector.Matches(nodeLabels) {
			matches = append(matches, selectors[i].name)
		}
	}
	return matches
}

func isReadyAndSchedulable(node corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for i := range node.Status.Conditions {
		condition := node.Status.Conditions[i]
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func classifyNamespaces(namespaces []corev1.Namespace) (map[string]string, int) {
	managed := make(map[string]string)
	invalid := 0
	for i := range namespaces {
		namespace := namespaces[i]
		if namespace.Labels[NamespaceEnforcedLabel] != "true" {
			continue
		}
		department := namespace.Labels[NamespaceDepartmentLabel]
		if department == "" {
			invalid++
			continue
		}
		managed[namespace.Name] = department
	}
	return managed, invalid
}

func resolvePodPool(
	pod corev1.Pod,
	classification *nodeClassification,
) (string, bool) {
	declaredPool := pod.Labels[PodResourcePoolLabel]
	_, declaredPoolExists := classification.pools[declaredPool]

	if pod.Spec.NodeName == "" {
		return declaredPool, declaredPool != "" && declaredPoolExists
	}

	actualPool, actualPoolExists := classification.poolByNode[pod.Spec.NodeName]
	if actualPoolExists {
		if declaredPool != "" && declaredPool != actualPool {
			classification.diagnostics.DeclaredPoolMismatches++
		}
		return actualPool, true
	}

	if declaredPool != "" && declaredPoolExists {
		classification.diagnostics.FallbackPoolPods++
		return declaredPool, true
	}

	return "", false
}

type departmentPoolKey struct {
	department string
	pool       string
}

func newSnapshot(
	classification nodeClassification,
	departmentLimits map[departmentPoolKey]corev1.ResourceList,
) Snapshot {
	pools := make([]Pool, 0, len(classification.pools))
	for _, pool := range classification.pools {
		pools = append(pools, clonePool(pool))
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })

	departments := make([]DepartmentPool, 0, len(departmentLimits))
	for key, limits := range departmentLimits {
		departments = append(departments, DepartmentPool{
			Department:      key.department,
			Pool:            key.pool,
			AllocatedLimits: quota.Clone(limits),
		})
	}
	sort.Slice(departments, func(i, j int) bool {
		if departments[i].Department == departments[j].Department {
			return departments[i].Pool < departments[j].Pool
		}
		return departments[i].Department < departments[j].Department
	})

	return Snapshot{
		pools:       pools,
		departments: departments,
		diagnostics: classification.diagnostics,
	}
}

func clonePool(pool Pool) Pool {
	pool.Allocatable = quota.Clone(pool.Allocatable)
	pool.AllocatedLimits = quota.Clone(pool.AllocatedLimits)
	return pool
}
