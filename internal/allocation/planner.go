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

// Package allocation calculates memory budgets and elastic department quotas.
package allocation

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
	"kgpp34.com/resource-elastic-quota/internal/quota"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PoolResult contains the calculated capacity budgets for one ResourcePool.
type PoolResult struct {
	PhysicalCapacity            corev1.ResourceList
	AdmissionBudget             corev1.ResourceList
	ConfiguredEntitlementBudget corev1.ResourceList
	EffectiveEntitlementBudget  corev1.ResourceList
	BaseQuotaOversubscribed     bool
	AdmissionOverCapacity       bool
	MemoryPressure              bool
	BorrowingBlocked            bool
}

// DepartmentResult contains one DepartmentQuota object's allocation in one pool.
type DepartmentResult struct {
	EffectiveQuota corev1.ResourceList
	Borrowed       corev1.ResourceList
	LastDemandTime *metav1.Time
	LastUpdateTime *metav1.Time
	OverQuota      bool
}

// Plan is immutable by convention. Accessors return deep copies.
type Plan struct {
	pools       map[string]PoolResult
	departments map[departmentKey]DepartmentResult
}

// Pool returns the result for a ResourcePool.
func (p Plan) Pool(name string) (PoolResult, bool) {
	result, exists := p.pools[name]
	if !exists {
		return PoolResult{}, false
	}
	return clonePoolResult(result), true
}

// Department returns one DepartmentQuota object's result in a ResourcePool.
func (p Plan) Department(objectName, poolName string) (DepartmentResult, bool) {
	result, exists := p.departments[departmentKey{objectName: objectName, poolName: poolName}]
	if !exists {
		return DepartmentResult{}, false
	}
	return cloneDepartmentResult(result), true
}

type departmentKey struct {
	objectName string
	poolName   string
}

type poolEntry struct {
	key          departmentKey
	department   string
	base         int64
	maximum      int64
	weight       int64
	target       int64
	headroom     int64
	reclaimAfter time.Duration
	allocated    int64
	current      *quotav1alpha1.DepartmentPoolQuotaStatus
	desired      int64
	raw          int64
	effective    int64
	lastDemand   *metav1.Time
	lastUpdate   *metav1.Time
}

// Calculate builds one deterministic memory-only allocation plan.
func Calculate(
	policy quotav1alpha1.ElasticQuotaPolicy,
	resourcePools []quotav1alpha1.ResourcePool,
	departmentQuotas []quotav1alpha1.DepartmentQuota,
	snapshot accounting.Snapshot,
	now time.Time,
) (Plan, error) {
	if err := validateUniqueDepartments(departmentQuotas); err != nil {
		return Plan{}, err
	}

	poolAccounting := make(map[string]accounting.Pool)
	for _, pool := range snapshot.Pools() {
		poolAccounting[pool.Name] = pool
	}
	departmentAccounting := make(map[string]map[string]int64)
	for _, item := range snapshot.DepartmentPools() {
		allocated, err := memoryBytes(item.AllocatedLimits, "allocated limits")
		if err != nil {
			return Plan{}, fmt.Errorf("department %q pool %q: %w", item.Department, item.Pool, err)
		}
		if departmentAccounting[item.Department] == nil {
			departmentAccounting[item.Department] = make(map[string]int64)
		}
		departmentAccounting[item.Department][item.Pool] = allocated
	}

	entriesByPool := make(map[string][]*poolEntry)
	knownPools := make(map[string]struct{}, len(resourcePools))
	for i := range resourcePools {
		knownPools[resourcePools[i].Name] = struct{}{}
	}
	for i := range departmentQuotas {
		departmentQuota := &departmentQuotas[i]
		currentByPool := currentStatusByPool(departmentQuota.Status.Pools)
		for j := range departmentQuota.Spec.Pools {
			configured := departmentQuota.Spec.Pools[j]
			if _, exists := knownPools[configured.Name]; !exists {
				continue
			}
			entry, err := newPoolEntry(*departmentQuota, configured, currentByPool[configured.Name], departmentAccounting)
			if err != nil {
				return Plan{}, err
			}
			entriesByPool[configured.Name] = append(entriesByPool[configured.Name], entry)
		}
	}

	result := Plan{
		pools:       make(map[string]PoolResult, len(resourcePools)),
		departments: make(map[departmentKey]DepartmentResult),
	}
	for i := range resourcePools {
		configuredPool := resourcePools[i]
		calculatedPool := poolAccounting[configuredPool.Name]
		entries := entriesByPool[configuredPool.Name]
		poolResult, err := calculatePool(
			policy.Spec.Allocation,
			policy.Spec.Pressure,
			configuredPool,
			calculatedPool,
			entries,
			now,
		)
		if err != nil {
			return Plan{}, fmt.Errorf("resource pool %q: %w", configuredPool.Name, err)
		}
		result.pools[configuredPool.Name] = poolResult
		for _, entry := range entries {
			result.departments[entry.key] = DepartmentResult{
				EffectiveQuota: memoryList(entry.effective),
				Borrowed:       memoryList(maxInt64(0, entry.effective-entry.base)),
				LastDemandTime: copyTime(entry.lastDemand),
				LastUpdateTime: copyTime(entry.lastUpdate),
				OverQuota:      entry.allocated > entry.effective,
			}
		}
	}
	return result, nil
}

func calculatePool(
	policy quotav1alpha1.AllocationPolicy,
	pressurePolicy quotav1alpha1.PressurePolicy,
	configured quotav1alpha1.ResourcePool,
	accounted accounting.Pool,
	entries []*poolEntry,
	now time.Time,
) (PoolResult, error) {
	allocatable, err := memoryBytes(accounted.Allocatable, "allocatable")
	if err != nil {
		return PoolResult{}, err
	}
	reserve, err := memoryBytes(configured.Spec.CapacityPolicy.Reserve, "reserve")
	if err != nil {
		return PoolResult{}, err
	}
	physical := maxInt64(0, allocatable-reserve)

	admissionRatio, err := memoryRatio(configured.Spec.CapacityPolicy.AdmissionRatio, "admission ratio")
	if err != nil {
		return PoolResult{}, err
	}
	entitlementRatio, err := memoryRatio(configured.Spec.CapacityPolicy.EntitlementRatio, "entitlement ratio")
	if err != nil {
		return PoolResult{}, err
	}
	admissionBudget := multiplyFloor(physical, admissionRatio)
	configuredEntitlement := multiplyFloor(physical, entitlementRatio)
	minIncrease, err := memoryBytes(policy.MinIncrease, "minimum increase")
	if err != nil {
		return PoolResult{}, err
	}
	blockBorrowing := borrowingBlocked(configured, accounted, pressurePolicy, now)

	var sumBase int64
	for _, entry := range entries {
		sumBase = checkedAdd(sumBase, entry.base)
		entry.desired = desiredQuota(entry, blockBorrowing)
	}
	effectiveEntitlement := maxInt64(configuredEntitlement, sumBase)
	allocateWeighted(entries, effectiveEntitlement-sumBase)
	applyHysteresis(entries, policy, minIncrease, effectiveEntitlement, now)

	allocated, err := memoryBytes(accounted.AllocatedLimits, "allocated limits")
	if err != nil {
		return PoolResult{}, err
	}
	return PoolResult{
		PhysicalCapacity:            memoryList(physical),
		AdmissionBudget:             memoryList(admissionBudget),
		ConfiguredEntitlementBudget: memoryList(configuredEntitlement),
		EffectiveEntitlementBudget:  memoryList(effectiveEntitlement),
		BaseQuotaOversubscribed:     sumBase > physical,
		AdmissionOverCapacity:       allocated > admissionBudget,
		MemoryPressure:              accounted.HasMemoryPressure,
		BorrowingBlocked:            blockBorrowing,
	}, nil
}

func borrowingBlocked(
	configured quotav1alpha1.ResourcePool,
	accounted accounting.Pool,
	policy quotav1alpha1.PressurePolicy,
	now time.Time,
) bool {
	if !policy.BlockBorrowingOnNodeMemoryPressure {
		return false
	}
	if accounted.HasMemoryPressure {
		return true
	}
	if policy.RecoveryWindow.Duration <= 0 {
		return false
	}
	var memoryCondition, blockedCondition *metav1.Condition
	for i := range configured.Status.Conditions {
		condition := &configured.Status.Conditions[i]
		switch condition.Type {
		case "MemoryPressure":
			memoryCondition = condition
		case "BorrowingBlocked":
			blockedCondition = condition
		}
	}
	if memoryCondition == nil {
		return false
	}
	// The first healthy observation after pressure keeps protection active so
	// the controller can record the recovery start as the condition transition.
	if memoryCondition.Status == metav1.ConditionTrue {
		return true
	}
	return blockedCondition != nil && blockedCondition.Status == metav1.ConditionTrue &&
		now.Sub(memoryCondition.LastTransitionTime.Time) < policy.RecoveryWindow.Duration
}

func newPoolEntry(
	departmentQuota quotav1alpha1.DepartmentQuota,
	configured quotav1alpha1.DepartmentPoolQuota,
	current *quotav1alpha1.DepartmentPoolQuotaStatus,
	allocatedByDepartment map[string]map[string]int64,
) (*poolEntry, error) {
	base, err := memoryBytes(configured.BaseQuota, "base quota")
	if err != nil {
		return nil, fmt.Errorf("department quota %q pool %q: %w", departmentQuota.Name, configured.Name, err)
	}
	maximum, err := memoryBytes(configured.MaxQuota, "max quota")
	if err != nil {
		return nil, fmt.Errorf("department quota %q pool %q: %w", departmentQuota.Name, configured.Name, err)
	}
	if base > maximum {
		return nil, fmt.Errorf("department quota %q pool %q: base quota exceeds max quota", departmentQuota.Name, configured.Name)
	}
	weight := int64(configured.Weight)
	if weight == 0 {
		weight = 100
	}
	if weight < 0 {
		return nil, fmt.Errorf("department quota %q pool %q: weight must be positive", departmentQuota.Name, configured.Name)
	}
	target := int64(configured.TargetUtilizationPercent)
	if target == 0 {
		target = 80
	}
	if target < 1 || target > 100 {
		return nil, fmt.Errorf("department quota %q pool %q: target utilization must be between 1 and 100", departmentQuota.Name, configured.Name)
	}
	headroom := int64(configured.GrowthHeadroomPercent)
	if headroom < 0 {
		return nil, fmt.Errorf("department quota %q pool %q: growth headroom must be non-negative", departmentQuota.Name, configured.Name)
	}

	allocated := int64(0)
	if pools := allocatedByDepartment[departmentQuota.Spec.Department]; pools != nil {
		allocated = pools[configured.Name]
	}
	return &poolEntry{
		key:          departmentKey{objectName: departmentQuota.Name, poolName: configured.Name},
		department:   departmentQuota.Spec.Department,
		base:         base,
		maximum:      maximum,
		weight:       weight,
		target:       target,
		headroom:     headroom,
		reclaimAfter: configured.ReclaimAfter.Duration,
		allocated:    allocated,
		current:      current,
	}, nil
}

func desiredQuota(entry *poolEntry, memoryPressure bool) int64 {
	desired := entry.base
	allocatedTarget := ceilMulDiv(entry.allocated, 100, entry.target)
	allocatedHeadroom := ceilMulDiv(entry.allocated, 100+entry.headroom, 100)
	if memoryPressure {
		allocatedTarget = entry.allocated
		allocatedHeadroom = entry.allocated
	}
	desired = maxInt64(desired, allocatedTarget)
	desired = maxInt64(desired, allocatedHeadroom)
	return minInt64(desired, entry.maximum)
}

// allocateWeighted implements progressive filling. Integer remainders are
// assigned by stable object/pool order, so identical inputs always converge.
func allocateWeighted(entries []*poolEntry, shared int64) {
	active := make([]*poolEntry, 0, len(entries))
	for _, entry := range entries {
		entry.raw = entry.base
		if entry.desired > entry.base {
			active = append(active, entry)
		}
	}
	sortEntries(active)
	for shared > 0 && len(active) > 0 {
		var totalWeight int64
		for _, entry := range active {
			totalWeight = checkedAdd(totalWeight, entry.weight)
		}

		var capped []*poolEntry
		for _, entry := range active {
			demand := entry.desired - entry.raw
			if new(big.Int).Mul(big.NewInt(demand), big.NewInt(totalWeight)).Cmp(
				new(big.Int).Mul(big.NewInt(shared), big.NewInt(entry.weight)),
			) <= 0 {
				capped = append(capped, entry)
			}
		}
		if len(capped) > 0 {
			cappedSet := make(map[*poolEntry]struct{}, len(capped))
			for _, entry := range capped {
				demand := entry.desired - entry.raw
				entry.raw += demand
				shared -= demand
				cappedSet[entry] = struct{}{}
			}
			next := active[:0]
			for _, entry := range active {
				if _, exists := cappedSet[entry]; !exists {
					next = append(next, entry)
				}
			}
			active = next
			continue
		}

		var distributed int64
		for _, entry := range active {
			share := mulDivFloor(shared, entry.weight, totalWeight)
			entry.raw += share
			distributed += share
		}
		remainder := shared - distributed
		for i := int64(0); i < remainder; i++ {
			active[i%int64(len(active))].raw++
		}
		shared = 0
	}
}

func applyHysteresis(
	entries []*poolEntry,
	policy quotav1alpha1.AllocationPolicy,
	minIncrease int64,
	budget int64,
	now time.Time,
) {
	nowTime := metav1.NewTime(now)
	for _, entry := range entries {
		if entry.current == nil || entry.current.EffectiveQuota == nil {
			entry.effective = entry.raw
			entry.lastDemand = &nowTime
			entry.lastUpdate = &nowTime
			continue
		}

		current, err := memoryBytes(entry.current.EffectiveQuota, "current effective quota")
		if err != nil {
			current = entry.base
		}
		current = minInt64(maxInt64(current, entry.base), entry.maximum)
		entry.effective = current
		entry.lastDemand = copyTime(entry.current.LastDemandTime)
		entry.lastUpdate = copyTime(entry.current.LastUpdateTime)
		previousAllocated, previousAllocatedErr := memoryBytes(
			entry.current.AllocatedLimits,
			"previous allocated limits",
		)
		allocatedChanged := previousAllocatedErr != nil || previousAllocated != entry.allocated

		switch {
		case entry.raw >= current:
			if entry.lastDemand == nil || allocatedChanged {
				entry.lastDemand = &nowTime
			}
			if entry.raw > current {
				if current == 0 && minIncrease == 0 {
					entry.effective = entry.raw
				} else {
					increase := ceilMulDiv(current, int64(policy.MaxIncreasePercent), 100)
					increase = maxInt64(increase, minIncrease)
					entry.effective = minInt64(entry.raw, checkedAdd(current, increase))
				}
			}
		case canReclaim(entry, policy.Cooldown.Duration, now):
			decrease := ceilMulDiv(current, int64(policy.MaxDecreasePercent), 100)
			entry.effective = maxInt64(entry.raw, current-decrease)
		case entry.lastDemand == nil:
			entry.lastDemand = &nowTime
		}
		if entry.effective != current {
			entry.lastUpdate = &nowTime
			if entry.effective > current {
				entry.lastDemand = &nowTime
			}
		}
	}

	// A capacity reduction is authoritative. Delayed scale-down may consume
	// spare budget, but it cannot leave the sum above the pool entitlement.
	var total, rawTotal int64
	for _, entry := range entries {
		total = checkedAdd(total, entry.effective)
		rawTotal = checkedAdd(rawTotal, entry.raw)
	}
	if total <= budget {
		return
	}
	retained := allocateRetainedQuota(entries, maxInt64(0, budget-rawTotal))
	for _, entry := range entries {
		previous := entry.effective
		entry.effective = checkedAdd(entry.raw, retained[entry])
		if entry.effective != previous {
			entry.lastUpdate = &nowTime
		}
	}
}

type retainedEntry struct {
	entry     *poolEntry
	capacity  int64
	allocated int64
}

func allocateRetainedQuota(entries []*poolEntry, shared int64) map[*poolEntry]int64 {
	result := make(map[*poolEntry]int64, len(entries))
	active := make([]*retainedEntry, 0, len(entries))
	for _, entry := range entries {
		capacity := entry.effective - entry.raw
		if capacity > 0 {
			active = append(active, &retainedEntry{entry: entry, capacity: capacity})
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].entry.key.objectName < active[j].entry.key.objectName
	})
	all := append([]*retainedEntry(nil), active...)
	for shared > 0 && len(active) > 0 {
		var totalWeight int64
		for _, item := range active {
			totalWeight = checkedAdd(totalWeight, item.entry.weight)
		}
		var capped []*retainedEntry
		for _, item := range active {
			demand := item.capacity - item.allocated
			if new(big.Int).Mul(big.NewInt(demand), big.NewInt(totalWeight)).Cmp(
				new(big.Int).Mul(big.NewInt(shared), big.NewInt(item.entry.weight)),
			) <= 0 {
				capped = append(capped, item)
			}
		}
		if len(capped) > 0 {
			cappedSet := make(map[*retainedEntry]struct{}, len(capped))
			for _, item := range capped {
				demand := item.capacity - item.allocated
				item.allocated += demand
				shared -= demand
				cappedSet[item] = struct{}{}
			}
			next := active[:0]
			for _, item := range active {
				if _, exists := cappedSet[item]; !exists {
					next = append(next, item)
				}
			}
			active = next
			continue
		}
		var distributed int64
		for _, item := range active {
			share := mulDivFloor(shared, item.entry.weight, totalWeight)
			item.allocated += share
			distributed += share
		}
		remainder := shared - distributed
		for i := int64(0); i < remainder; i++ {
			active[i%int64(len(active))].allocated++
		}
		shared = 0
	}
	for _, item := range all {
		result[item.entry] = item.allocated
	}
	return result
}

func canReclaim(entry *poolEntry, cooldown time.Duration, now time.Time) bool {
	if entry.lastDemand == nil || now.Sub(entry.lastDemand.Time) < entry.reclaimAfter {
		return false
	}
	return entry.lastUpdate == nil || now.Sub(entry.lastUpdate.Time) >= cooldown
}

func validateUniqueDepartments(departmentQuotas []quotav1alpha1.DepartmentQuota) error {
	seen := make(map[string]string, len(departmentQuotas))
	for i := range departmentQuotas {
		department := departmentQuotas[i].Spec.Department
		if previous, exists := seen[department]; exists {
			return fmt.Errorf("department %q is configured by both %q and %q", department, previous, departmentQuotas[i].Name)
		}
		seen[department] = departmentQuotas[i].Name
	}
	return nil
}

func currentStatusByPool(statuses []quotav1alpha1.DepartmentPoolQuotaStatus) map[string]*quotav1alpha1.DepartmentPoolQuotaStatus {
	result := make(map[string]*quotav1alpha1.DepartmentPoolQuotaStatus, len(statuses))
	for i := range statuses {
		status := statuses[i].DeepCopy()
		result[status.Name] = status
	}
	return result
}

func memoryBytes(resources corev1.ResourceList, field string) (int64, error) {
	for name := range resources {
		if name != corev1.ResourceMemory {
			return 0, fmt.Errorf("%s contains unsupported resource %q", field, name)
		}
	}
	quantity, exists := resources[corev1.ResourceMemory]
	if !exists {
		return 0, nil
	}
	if quantity.Sign() < 0 {
		return 0, fmt.Errorf("%s memory must be non-negative", field)
	}
	return quantity.Value(), nil
}

func memoryRatio(resources corev1.ResourceList, field string) (*big.Rat, error) {
	for name := range resources {
		if name != corev1.ResourceMemory {
			return nil, fmt.Errorf("%s contains unsupported resource %q", field, name)
		}
	}
	quantity, exists := resources[corev1.ResourceMemory]
	if !exists {
		return big.NewRat(1, 1), nil
	}
	if quantity.Sign() < 0 {
		return nil, fmt.Errorf("%s must be non-negative", field)
	}
	ratio, ok := new(big.Rat).SetString(quantity.AsDec().String())
	if !ok {
		return nil, fmt.Errorf("%s %q is invalid", field, quantity.String())
	}
	return ratio, nil
}

func multiplyFloor(value int64, factor *big.Rat) int64 {
	numerator := new(big.Int).Mul(big.NewInt(value), factor.Num())
	result := new(big.Int).Quo(numerator, factor.Denom())
	if !result.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return result.Int64()
}

func mulDivFloor(value, multiplier, divisor int64) int64 {
	result := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	result.Quo(result, big.NewInt(divisor))
	if !result.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return result.Int64()
}

func ceilMulDiv(value, multiplier, divisor int64) int64 {
	if value == 0 || multiplier == 0 {
		return 0
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	numerator.Add(numerator, big.NewInt(divisor-1))
	numerator.Quo(numerator, big.NewInt(divisor))
	if !numerator.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return numerator.Int64()
}

func checkedAdd(left, right int64) int64 {
	result := new(big.Int).Add(big.NewInt(left), big.NewInt(right))
	if !result.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return result.Int64()
}

func memoryList(bytes int64) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(bytes, resource.BinarySI)}
}

func sortEntries(entries []*poolEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key.objectName == entries[j].key.objectName {
			return entries[i].key.poolName < entries[j].key.poolName
		}
		return entries[i].key.objectName < entries[j].key.objectName
	})
}

func clonePoolResult(result PoolResult) PoolResult {
	result.PhysicalCapacity = quota.Clone(result.PhysicalCapacity)
	result.AdmissionBudget = quota.Clone(result.AdmissionBudget)
	result.ConfiguredEntitlementBudget = quota.Clone(result.ConfiguredEntitlementBudget)
	result.EffectiveEntitlementBudget = quota.Clone(result.EffectiveEntitlementBudget)
	return result
}

func cloneDepartmentResult(result DepartmentResult) DepartmentResult {
	result.EffectiveQuota = quota.Clone(result.EffectiveQuota)
	result.Borrowed = quota.Clone(result.Borrowed)
	result.LastDemandTime = copyTime(result.LastDemandTime)
	result.LastUpdateTime = copyTime(result.LastUpdateTime)
	return result
}

func copyTime(value *metav1.Time) *metav1.Time {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
