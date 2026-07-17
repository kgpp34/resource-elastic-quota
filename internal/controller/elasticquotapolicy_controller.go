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
	"fmt"
	"sort"
	"time"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
	"kgpp34.com/resource-elastic-quota/internal/allocation"
	"kgpp34.com/resource-elastic-quota/internal/quota"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// DefaultPolicyName is the singleton policy reconciled by all cluster events.
	DefaultPolicyName = "default"
	defaultFullResync = 5 * time.Minute
)

// ElasticQuotaPolicyReconciler rebuilds cluster accounting and publishes it to
// ResourcePool and DepartmentQuota status.
type ElasticQuotaPolicyReconciler struct {
	client.Client
	FullResyncInterval time.Duration
	Now                func() time.Time
}

// Reconcile performs one synchronous full calculation. controller-runtime owns
// concurrency and cancellation; this reconciler does not start background goroutines.
func (r *ElasticQuotaPolicyReconciler) Reconcile(
	ctx context.Context,
	request ctrl.Request,
) (ctrl.Result, error) {
	if request.Name != DefaultPolicyName {
		return ctrl.Result{}, nil
	}

	policy := &quotav1alpha1.ElasticQuotaPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: DefaultPolicyName}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get elastic quota policy %q: %w", DefaultPolicyName, err)
	}

	resourcePools := &quotav1alpha1.ResourcePoolList{}
	if err := r.List(ctx, resourcePools); err != nil {
		return ctrl.Result{}, fmt.Errorf("list resource pools: %w", err)
	}
	departmentQuotas := &quotav1alpha1.DepartmentQuotaList{}
	if err := r.List(ctx, departmentQuotas); err != nil {
		return ctrl.Result{}, fmt.Errorf("list department quotas: %w", err)
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	namespaces := &corev1.NamespaceList{}
	if err := r.List(ctx, namespaces); err != nil {
		return ctrl.Result{}, fmt.Errorf("list namespaces: %w", err)
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods); err != nil {
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}

	snapshot, err := accounting.Calculate(
		resourcePools.Items,
		nodes.Items,
		namespaces.Items,
		pods.Items,
	)
	if err != nil {
		if statusErr := r.patchPolicyFailure(ctx, policy, err); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("calculate accounting: %v; patch policy failure: %w", err, statusErr)
		}
		return ctrl.Result{}, fmt.Errorf("calculate accounting: %w", err)
	}

	calculationTime := r.now()
	allocationPlan, err := allocation.Calculate(
		*policy,
		resourcePools.Items,
		departmentQuotas.Items,
		snapshot,
		calculationTime,
	)
	if err != nil {
		if statusErr := r.patchPolicyFailure(ctx, policy, err); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("calculate allocation: %v; patch policy failure: %w", err, statusErr)
		}
		return ctrl.Result{}, fmt.Errorf("calculate allocation: %w", err)
	}

	now := metav1.NewTime(calculationTime)
	if err := r.patchResourcePools(ctx, resourcePools.Items, snapshot, allocationPlan, now); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchDepartmentQuotas(
		ctx,
		departmentQuotas.Items,
		resourcePools.Items,
		namespaces.Items,
		snapshot,
		allocationPlan,
		now,
	); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchPolicySuccess(ctx, policy, snapshot.Diagnostics(), now); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.fullResyncInterval(policy)}, nil
}

// SetupWithManager watches every object that affects the global accounting snapshot.
func (r *ElasticQuotaPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToDefaultPolicy := handler.EnqueueRequestsFromMapFunc(func(client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: DefaultPolicyName}}}
	})
	generationChanged := builder.WithPredicates(predicate.GenerationChangedPredicate{})

	return ctrl.NewControllerManagedBy(mgr).
		For(&quotav1alpha1.ElasticQuotaPolicy{}, generationChanged).
		Watches(&source.Kind{Type: &corev1.Pod{}}, mapToDefaultPolicy).
		Watches(&source.Kind{Type: &corev1.Node{}}, mapToDefaultPolicy).
		Watches(&source.Kind{Type: &corev1.Namespace{}}, mapToDefaultPolicy).
		Watches(&source.Kind{Type: &quotav1alpha1.ResourcePool{}}, mapToDefaultPolicy, generationChanged).
		Watches(&source.Kind{Type: &quotav1alpha1.DepartmentQuota{}}, mapToDefaultPolicy, generationChanged).
		Complete(r)
}

func (r *ElasticQuotaPolicyReconciler) patchResourcePools(
	ctx context.Context,
	resourcePools []quotav1alpha1.ResourcePool,
	snapshot accounting.Snapshot,
	allocationPlan allocation.Plan,
	now metav1.Time,
) error {
	calculatedPools := make(map[string]accounting.Pool)
	for _, pool := range snapshot.Pools() {
		calculatedPools[pool.Name] = pool
	}

	for i := range resourcePools {
		resourcePool := &resourcePools[i]
		calculated := calculatedPools[resourcePool.Name]
		original := resourcePool.DeepCopy()

		resourcePool.Status.ObservedGeneration = resourcePool.Generation
		resourcePool.Status.ReadyNodes = calculated.ReadyNodes
		resourcePool.Status.Allocatable = quota.Clone(calculated.Allocatable)
		resourcePool.Status.AllocatedLimits = quota.Clone(calculated.AllocatedLimits)
		allocated, _ := allocationPlan.Pool(resourcePool.Name)
		resourcePool.Status.AdmissionBudget = quota.Clone(allocated.AdmissionBudget)
		resourcePool.Status.ConfiguredEntitlementBudget = quota.Clone(allocated.ConfiguredEntitlementBudget)
		resourcePool.Status.EffectiveEntitlementBudget = quota.Clone(allocated.EffectiveEntitlementBudget)

		readyStatus := metav1.ConditionTrue
		readyReason := "CapacityAvailable"
		readyMessage := "resource pool has ready schedulable nodes"
		if calculated.ReadyNodes == 0 {
			readyStatus = metav1.ConditionFalse
			readyReason = "NoReadyNodes"
			readyMessage = "resource pool has no ready schedulable nodes"
		}
		setCondition(&resourcePool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             readyStatus,
			Reason:             readyReason,
			Message:            readyMessage,
			ObservedGeneration: resourcePool.Generation,
			LastTransitionTime: now,
		})

		setBooleanCondition(
			&resourcePool.Status.Conditions,
			"BaseQuotaOversubscribed",
			allocated.BaseQuotaOversubscribed,
			"BaseQuotaExceedsPhysicalCapacity",
			"BaseQuotaWithinPhysicalCapacity",
			"the sum of department base quotas exceeds physical memory capacity",
			"the sum of department base quotas is within physical memory capacity",
			resourcePool.Generation,
			now,
		)
		setBooleanCondition(
			&resourcePool.Status.Conditions,
			"AdmissionOverCapacity",
			allocated.AdmissionOverCapacity,
			"AllocatedLimitsExceedAdmissionBudget",
			"AllocatedLimitsWithinAdmissionBudget",
			"allocated memory limits exceed the admission budget",
			"allocated memory limits are within the admission budget",
			resourcePool.Generation,
			now,
		)
		setBooleanCondition(
			&resourcePool.Status.Conditions,
			"MemoryPressure",
			allocated.MemoryPressure,
			"NodeReportsMemoryPressure",
			"NoNodeMemoryPressure",
			"at least one node reports MemoryPressure; new headroom borrowing is suspended",
			"no node in the resource pool reports MemoryPressure",
			resourcePool.Generation,
			now,
		)
		setBooleanCondition(
			&resourcePool.Status.Conditions,
			"BorrowingBlocked",
			allocated.BorrowingBlocked,
			"MemoryPressureProtectionActive",
			"BorrowingAllowed",
			"new quota headroom borrowing is suspended by memory-pressure protection",
			"memory-pressure protection allows quota borrowing",
			resourcePool.Generation,
			now,
		)

		overlapStatus := metav1.ConditionFalse
		overlapReason := "SelectorsUnique"
		overlapMessage := "no node matches multiple resource pools"
		if calculated.HasSelectorOverlap {
			overlapStatus = metav1.ConditionTrue
			overlapReason = "NodeMatchesMultiplePools"
			overlapMessage = "at least one node matches multiple resource pools and provides no capacity"
		}
		setCondition(&resourcePool.Status.Conditions, metav1.Condition{
			Type:               "SelectorOverlap",
			Status:             overlapStatus,
			Reason:             overlapReason,
			Message:            overlapMessage,
			ObservedGeneration: resourcePool.Generation,
			LastTransitionTime: now,
		})

		if equality.Semantic.DeepEqual(original.Status, resourcePool.Status) {
			continue
		}
		if err := r.Status().Patch(ctx, resourcePool, client.MergeFrom(original)); err != nil {
			return fmt.Errorf("patch resource pool %q status: %w", resourcePool.Name, err)
		}
	}
	return nil
}

func (r *ElasticQuotaPolicyReconciler) patchDepartmentQuotas(
	ctx context.Context,
	departmentQuotas []quotav1alpha1.DepartmentQuota,
	resourcePools []quotav1alpha1.ResourcePool,
	namespaces []corev1.Namespace,
	snapshot accounting.Snapshot,
	allocationPlan allocation.Plan,
	now metav1.Time,
) error {
	poolExists := make(map[string]struct{}, len(resourcePools))
	for i := range resourcePools {
		poolExists[resourcePools[i].Name] = struct{}{}
	}
	departmentCounts := make(map[string]int, len(departmentQuotas))
	for i := range departmentQuotas {
		departmentCounts[departmentQuotas[i].Spec.Department]++
	}
	allocatedByDepartment := make(map[string]map[string]corev1.ResourceList)
	for _, departmentPool := range snapshot.DepartmentPools() {
		if allocatedByDepartment[departmentPool.Department] == nil {
			allocatedByDepartment[departmentPool.Department] = make(map[string]corev1.ResourceList)
		}
		allocatedByDepartment[departmentPool.Department][departmentPool.Pool] =
			quota.Clone(departmentPool.AllocatedLimits)
	}

	for i := range departmentQuotas {
		departmentQuota := &departmentQuotas[i]
		original := departmentQuota.DeepCopy()
		departmentQuota.Status.ObservedGeneration = departmentQuota.Generation

		configuredPools := make(map[string]struct{}, len(departmentQuota.Spec.Pools))
		unknownPool := ""
		for _, configured := range departmentQuota.Spec.Pools {
			configuredPools[configured.Name] = struct{}{}
			if _, exists := poolExists[configured.Name]; !exists && unknownPool == "" {
				unknownPool = configured.Name
			}
		}

		allocated := allocatedByDepartment[departmentQuota.Spec.Department]
		unconfiguredPool := ""
		for poolName := range allocated {
			if _, configured := configuredPools[poolName]; !configured && unconfiguredPool == "" {
				unconfiguredPool = poolName
			}
		}

		departmentQuota.Status.Pools = mergeDepartmentPoolStatus(
			departmentQuota.Status.Pools,
			departmentQuota.Name,
			configuredPools,
			allocated,
			allocationPlan,
		)

		configurationStatus := metav1.ConditionTrue
		configurationReason := "ConfigurationValid"
		configurationMessage := "department and resource pool references are valid"
		namespaceSelectorReason, namespaceSelectorMessage := validateNamespaceSelector(
			*departmentQuota,
			namespaces,
		)
		switch {
		case departmentCounts[departmentQuota.Spec.Department] > 1:
			configurationStatus = metav1.ConditionFalse
			configurationReason = "DuplicateDepartment"
			configurationMessage = "multiple DepartmentQuota objects use the same department"
		case namespaceSelectorReason != "":
			configurationStatus = metav1.ConditionFalse
			configurationReason = namespaceSelectorReason
			configurationMessage = namespaceSelectorMessage
		case unknownPool != "":
			configurationStatus = metav1.ConditionFalse
			configurationReason = "UnknownResourcePool"
			configurationMessage = fmt.Sprintf("configured resource pool %q does not exist", unknownPool)
		case unconfiguredPool != "":
			configurationStatus = metav1.ConditionFalse
			configurationReason = "UnconfiguredAllocation"
			configurationMessage = fmt.Sprintf("department has pods in unconfigured resource pool %q", unconfiguredPool)
		}
		setCondition(&departmentQuota.Status.Conditions, metav1.Condition{
			Type:               "ConfigurationValid",
			Status:             configurationStatus,
			Reason:             configurationReason,
			Message:            configurationMessage,
			ObservedGeneration: departmentQuota.Generation,
			LastTransitionTime: now,
		})
		overQuota := false
		for _, configured := range departmentQuota.Spec.Pools {
			if result, exists := allocationPlan.Department(departmentQuota.Name, configured.Name); exists && result.OverQuota {
				overQuota = true
				break
			}
		}
		setBooleanCondition(
			&departmentQuota.Status.Conditions,
			"OverQuota",
			overQuota,
			"AllocatedLimitsExceedEffectiveQuota",
			"AllocatedLimitsWithinEffectiveQuota",
			"allocated memory limits exceed the current effective quota; existing Pods are not evicted",
			"allocated memory limits are within the current effective quota",
			departmentQuota.Generation,
			now,
		)
		setCondition(&departmentQuota.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             configurationStatus,
			Reason:             configurationReason,
			Message:            configurationMessage,
			ObservedGeneration: departmentQuota.Generation,
			LastTransitionTime: now,
		})

		if equality.Semantic.DeepEqual(original.Status, departmentQuota.Status) {
			continue
		}
		if err := r.Status().Patch(ctx, departmentQuota, client.MergeFrom(original)); err != nil {
			return fmt.Errorf("patch department quota %q status: %w", departmentQuota.Name, err)
		}
	}
	return nil
}

func mergeDepartmentPoolStatus(
	current []quotav1alpha1.DepartmentPoolQuotaStatus,
	departmentQuotaName string,
	configuredPools map[string]struct{},
	allocated map[string]corev1.ResourceList,
	allocationPlan allocation.Plan,
) []quotav1alpha1.DepartmentPoolQuotaStatus {
	currentByName := make(map[string]quotav1alpha1.DepartmentPoolQuotaStatus, len(current))
	for i := range current {
		currentByName[current[i].Name] = *current[i].DeepCopy()
	}
	poolNames := make(map[string]struct{}, len(configuredPools)+len(allocated))
	for poolName := range configuredPools {
		poolNames[poolName] = struct{}{}
	}
	for poolName := range allocated {
		poolNames[poolName] = struct{}{}
	}

	result := make([]quotav1alpha1.DepartmentPoolQuotaStatus, 0, len(poolNames))
	for poolName := range poolNames {
		status := currentByName[poolName]
		status.Name = poolName
		status.AllocatedLimits = quota.Clone(allocated[poolName])
		if planned, exists := allocationPlan.Department(departmentQuotaName, poolName); exists {
			status.EffectiveQuota = quota.Clone(planned.EffectiveQuota)
			status.Borrowed = quota.Clone(planned.Borrowed)
			status.LastDemandTime = planned.LastDemandTime
			status.LastUpdateTime = planned.LastUpdateTime
		} else if _, configured := configuredPools[poolName]; configured {
			status.EffectiveQuota = corev1.ResourceList{}
			status.Borrowed = corev1.ResourceList{}
			status.LastDemandTime = nil
			status.LastUpdateTime = nil
		}
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func setBooleanCondition(
	conditions *[]metav1.Condition,
	conditionType string,
	active bool,
	trueReason string,
	falseReason string,
	trueMessage string,
	falseMessage string,
	observedGeneration int64,
	now metav1.Time,
) {
	status := metav1.ConditionFalse
	reason := falseReason
	message := falseMessage
	if active {
		status = metav1.ConditionTrue
		reason = trueReason
		message = trueMessage
	}
	setCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: now,
	})
}

func validateNamespaceSelector(
	departmentQuota quotav1alpha1.DepartmentQuota,
	namespaces []corev1.Namespace,
) (string, string) {
	selector, err := metav1.LabelSelectorAsSelector(&departmentQuota.Spec.NamespaceSelector)
	if err != nil {
		return "InvalidNamespaceSelector", fmt.Sprintf("namespace selector is invalid: %v", err)
	}
	for i := range namespaces {
		namespace := namespaces[i]
		if namespace.Labels[accounting.NamespaceEnforcedLabel] != "true" {
			continue
		}
		department := namespace.Labels[accounting.NamespaceDepartmentLabel]
		if department == "" {
			continue
		}
		matches := selector.Matches(labels.Set(namespace.Labels))
		switch {
		case department == departmentQuota.Spec.Department && !matches:
			return "NamespaceSelectorMismatch", fmt.Sprintf(
				"managed namespace %q belongs to the department but does not match namespaceSelector",
				namespace.Name,
			)
		case department != departmentQuota.Spec.Department && matches:
			return "NamespaceSelectorCrossesDepartments", fmt.Sprintf(
				"namespaceSelector also matches namespace %q from department %q",
				namespace.Name,
				department,
			)
		}
	}
	return "", ""
}

func (r *ElasticQuotaPolicyReconciler) patchPolicySuccess(
	ctx context.Context,
	policy *quotav1alpha1.ElasticQuotaPolicy,
	diagnostics accounting.Diagnostics,
	now metav1.Time,
) error {
	original := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.LastCalculationTime = &now
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "CalculationSucceeded",
		Message:            "resource accounting calculation completed",
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	})

	degradedStatus := metav1.ConditionFalse
	degradedReason := "AllObjectsClassified"
	degradedMessage := "all managed objects were classified"
	if diagnostics.IsDegraded() {
		degradedStatus = metav1.ConditionTrue
		degradedReason = "ClassificationIncomplete"
		degradedMessage = fmt.Sprintf(
			"overlappingNodes=%d invalidNamespaces=%d fallbackPoolPods=%d unresolvedPods=%d poolMismatches=%d",
			diagnostics.OverlappingNodes,
			diagnostics.InvalidNamespaces,
			diagnostics.FallbackPoolPods,
			diagnostics.UnresolvedPods,
			diagnostics.DeclaredPoolMismatches,
		)
	}
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "AccountingDegraded",
		Status:             degradedStatus,
		Reason:             degradedReason,
		Message:            degradedMessage,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	})

	unmatchedStatus := metav1.ConditionFalse
	unmatchedReason := "AllNodesMatched"
	unmatchedMessage := "all nodes match exactly one resource pool"
	if diagnostics.UnmatchedNodes > 0 {
		unmatchedStatus = metav1.ConditionTrue
		unmatchedReason = "NodesOutsideResourcePools"
		unmatchedMessage = fmt.Sprintf(
			"%d nodes do not match any resource pool and provide no managed capacity",
			diagnostics.UnmatchedNodes,
		)
	}
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "UnmatchedNodes",
		Status:             unmatchedStatus,
		Reason:             unmatchedReason,
		Message:            unmatchedMessage,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	})

	if equality.Semantic.DeepEqual(original.Status, policy.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch elastic quota policy %q status: %w", policy.Name, err)
	}
	return nil
}

func (r *ElasticQuotaPolicyReconciler) patchPolicyFailure(
	ctx context.Context,
	policy *quotav1alpha1.ElasticQuotaPolicy,
	calculationErr error,
) error {
	original := policy.DeepCopy()
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "CalculationFailed",
		Message:            calculationErr.Error(),
		ObservedGeneration: policy.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	if equality.Semantic.DeepEqual(original.Status, policy.Status) {
		return nil
	}
	return r.Status().Patch(ctx, policy, client.MergeFrom(original))
}

func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	meta.SetStatusCondition(conditions, condition)
	sort.Slice(*conditions, func(i, j int) bool { return (*conditions)[i].Type < (*conditions)[j].Type })
}

func (r *ElasticQuotaPolicyReconciler) fullResyncInterval(
	policy *quotav1alpha1.ElasticQuotaPolicy,
) time.Duration {
	if r.FullResyncInterval > 0 {
		return r.FullResyncInterval
	}
	fullResync := policy.Spec.Allocation.FullResync.Duration
	if fullResync <= 0 {
		fullResync = defaultFullResync
	}
	allocationInterval := time.Duration(policy.Spec.Allocation.IntervalSeconds) * time.Second
	if allocationInterval > 0 && allocationInterval < fullResync {
		return allocationInterval
	}
	return fullResync
}

func (r *ElasticQuotaPolicyReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}
