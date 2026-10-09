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

// Package webhook implements validating admission for department memory quotas.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	quotav1alpha1 "kgpp34.com/resource-elastic-quota/api/v1alpha1"
	"kgpp34.com/resource-elastic-quota/internal/accounting"
	"kgpp34.com/resource-elastic-quota/internal/quota"
)

const (
	maxAdmissionWarningLength = 256
	// ValidationPath is the HTTPS endpoint referenced by the webhook configuration.
	ValidationPath = "/validate-quota-kgpp34-io-v1-workloads"
)

// Handler validates Pods and supported built-in workloads against quota status.
type Handler struct {
	reader    client.Reader
	projector Projector
	now       func() time.Time
	log       logr.Logger
	recorder  AdmissionRecorder
}

// AdmissionRecorder is implemented by the optional Prometheus telemetry sink.
type AdmissionRecorder interface {
	ObserveAdmission(mode, result, reason string)
}

var _ admission.Handler = (*Handler)(nil)

// NewHandler constructs an admission handler with explicit dependencies.
func NewHandler(
	reader client.Reader,
	projector Projector,
	logger logr.Logger,
	recorders ...AdmissionRecorder,
) *Handler {
	handler := &Handler{
		reader:    reader,
		projector: projector,
		now:       time.Now,
		log:       logger,
	}
	if len(recorders) > 0 {
		handler.recorder = recorders[0]
	}
	return handler
}

// Handle validates one admission request without mutating cluster state.
func (h *Handler) Handle(ctx context.Context, request admission.Request) admission.Response {
	if request.Operation == admissionv1.Delete || request.SubResource != "" {
		return admission.Allowed("quota validation does not block deletion or subresource updates")
	}
	if request.Operation != admissionv1.Create && request.Operation != admissionv1.Update {
		return admission.Allowed("operation does not increase quota")
	}

	namespace := &corev1.Namespace{}
	if err := h.reader.Get(ctx, types.NamespacedName{Name: request.Namespace}, namespace); err != nil {
		return h.internalError(err, "read namespace admission state")
	}
	if namespace.Labels[accounting.NamespaceEnforcedLabel] != "true" {
		return admission.Allowed("namespace is not managed by department quota")
	}
	department := namespace.Labels[accounting.NamespaceDepartmentLabel]
	if department == "" {
		return denied("managed namespace has no platform department binding")
	}

	policy := &quotav1alpha1.ElasticQuotaPolicy{}
	if err := h.reader.Get(ctx, types.NamespacedName{Name: "default"}, policy); err != nil {
		return h.internalError(err, "read elastic quota policy admission state")
	}
	mode := policy.Spec.Admission.Mode
	if mode == "" {
		mode = quotav1alpha1.AdmissionModeObserve
	}

	kind := schema.GroupVersionKind{
		Group:   request.Kind.Group,
		Version: request.Kind.Version,
		Kind:    request.Kind.Kind,
	}
	newProjection, err := h.projector.Project(ctx, kind, request.Object)
	if err != nil {
		return h.internalError(err, "project admission object")
	}
	oldProjection := Projection{}
	if request.Operation == admissionv1.Update {
		oldProjection, err = h.projector.Project(ctx, kind, request.OldObject)
		if err != nil {
			return h.internalError(err, "project old admission object")
		}
	}

	delta := positiveDelta(newProjection, oldProjection, request.Operation)
	structureChanged := request.Operation == admissionv1.Create || projectionStructureChanged(newProjection, oldProjection)
	issues := make([]admissionIssue, 0, 4)
	if structureChanged || memoryValue(delta) > 0 {
		issues = append(issues, h.validateIdentityAndPlacement(ctx, department, newProjection, policy)...)
	}
	if memoryValue(delta) > 0 {
		issues = append(issues, h.validateQuota(ctx, department, newProjection.Pool, delta, policy)...)
	}
	response := respond(mode, issues)
	if h.recorder != nil {
		result := "allowed"
		reason := "within_quota"
		if len(issues) > 0 {
			reason = "validation_issue"
			if mode == quotav1alpha1.AdmissionModeEnforce && hasBlockingIssue(issues) {
				result = "denied"
			} else if mode == quotav1alpha1.AdmissionModeWarn {
				result = "warned"
			}
		}
		h.recorder.ObserveAdmission(string(mode), result, reason)
	}
	return response
}

func hasBlockingIssue(issues []admissionIssue) bool {
	for i := range issues {
		if issues[i].blockInEnforce {
			return true
		}
	}
	return false
}

type admissionIssue struct {
	message        string
	blockInEnforce bool
}

func (h *Handler) validateIdentityAndPlacement(
	ctx context.Context,
	department string,
	projection Projection,
	policy *quotav1alpha1.ElasticQuotaPolicy,
) []admissionIssue {
	var issues []admissionIssue
	if !projection.HasCompleteMemoryLimits {
		issues = append(issues, blockingIssue(
			"every regular and init container in a managed Pod template must declare a positive limits.memory",
		))
	}
	if projection.Department == "" {
		issues = append(issues, blockingIssue(fmt.Sprintf(
			"department %s requires Pod template label %s=%s",
			department,
			PodDepartmentLabel,
			department,
		)))
	} else if projection.Department != department {
		issues = append(issues, blockingIssue(fmt.Sprintf(
			"Pod template department %s does not match Namespace department %s",
			projection.Department,
			department,
		)))
	}
	if projection.Pool == "" {
		issues = append(issues, blockingIssue(fmt.Sprintf(
			"department %s requires Pod template label %s",
			department,
			accounting.PodResourcePoolLabel,
		)))
		return issues
	}

	pool := &quotav1alpha1.ResourcePool{}
	if err := h.reader.Get(ctx, types.NamespacedName{Name: projection.Pool}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			issues = append(issues, admissionIssue{
				message: fmt.Sprintf(
					"department %s references unknown resource pool %s",
					department,
					projection.Pool,
				),
				blockInEnforce: policy.Spec.Admission.FailClosedOnUnknownPool,
			})
			return issues
		}
		h.log.Error(err, "read resource pool for admission", "resourcePool", projection.Pool)
		issues = append(issues, blockingIssue("quota admission state is temporarily unavailable"))
		return issues
	}
	if err := validatePoolPlacement(ctx, h.reader, projection.PodSpec, *pool); err != nil {
		issues = append(issues, blockingIssue(fmt.Sprintf(
			"department %s resource pool %s placement is invalid: %s",
			department,
			projection.Pool,
			err.Error(),
		)))
	}
	return issues
}

func (h *Handler) validateQuota(
	ctx context.Context,
	department string,
	poolName string,
	delta corev1.ResourceList,
	policy *quotav1alpha1.ElasticQuotaPolicy,
) []admissionIssue {
	issues := make([]admissionIssue, 0, 4)
	if stale, age := snapshotStale(policy, h.currentTime()); stale {
		issues = append(issues, admissionIssue{
			message: fmt.Sprintf(
				"department %s quota snapshot is stale (age %s)",
				department,
				age,
			),
			blockInEnforce: policy.Spec.Admission.FailClosedOnStaleSnapshot,
		})
	}
	pool := &quotav1alpha1.ResourcePool{}
	if err := h.reader.Get(ctx, types.NamespacedName{Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return append(issues, admissionIssue{
				message:        fmt.Sprintf("resource pool %s is unknown", poolName),
				blockInEnforce: policy.Spec.Admission.FailClosedOnUnknownPool,
			})
		}
		h.log.Error(err, "read resource pool budget for admission", "resourcePool", poolName)
		return append(issues, blockingIssue("quota admission state is temporarily unavailable"))
	}

	departmentQuotas := &quotav1alpha1.DepartmentQuotaList{}
	if err := h.reader.List(ctx, departmentQuotas); err != nil {
		h.log.Error(err, "list department quotas for admission")
		return append(issues, blockingIssue("quota admission state is temporarily unavailable"))
	}
	matching := make([]quotav1alpha1.DepartmentQuota, 0, 1)
	for i := range departmentQuotas.Items {
		if departmentQuotas.Items[i].Spec.Department == department {
			matching = append(matching, departmentQuotas.Items[i])
		}
	}
	if len(matching) != 1 {
		return append(issues, blockingIssue(fmt.Sprintf(
			"department %s has %d DepartmentQuota configurations; exactly one is required",
			department,
			len(matching),
		)))
	}
	departmentQuota := matching[0]
	configured, configuredExists := configuredPool(departmentQuota, poolName)
	status, statusExists := departmentPoolStatus(departmentQuota, poolName)
	if !configuredExists || !statusExists {
		return append(issues, blockingIssue(fmt.Sprintf(
			"department %s has no ready quota for resource pool %s",
			department,
			poolName,
		)))
	}

	departmentAfter := quota.Add(status.AllocatedLimits, delta)
	if !quota.LessThanOrEqual(departmentAfter, status.EffectiveQuota) {
		issues = append(issues, blockingIssue(quotaExceededMessage(
			department,
			poolName,
			"effectiveQuota",
			status.AllocatedLimits,
			delta,
			status.EffectiveQuota,
		)))
	}
	if !quota.LessThanOrEqual(departmentAfter, configured.MaxQuota) {
		issues = append(issues, blockingIssue(quotaExceededMessage(
			department,
			poolName,
			"maxQuota",
			status.AllocatedLimits,
			delta,
			configured.MaxQuota,
		)))
	}

	poolAfter := quota.Add(pool.Status.AllocatedLimits, delta)
	if !quota.LessThanOrEqual(poolAfter, pool.Status.AdmissionBudget) {
		issues = append(issues, blockingIssue(quotaExceededMessage(
			department,
			poolName,
			"admissionBudget",
			pool.Status.AllocatedLimits,
			delta,
			pool.Status.AdmissionBudget,
		)))
	}
	return issues
}

func positiveDelta(newProjection, oldProjection Projection, operation admissionv1.Operation) corev1.ResourceList {
	if operation == admissionv1.Create || newProjection.Pool != oldProjection.Pool {
		return quota.Clone(newProjection.Limits)
	}
	return quota.ClampNonNegative(quota.Subtract(newProjection.Limits, oldProjection.Limits))
}

func projectionStructureChanged(current, previous Projection) bool {
	return current.Department != previous.Department ||
		current.Pool != previous.Pool ||
		current.HasCompleteMemoryLimits != previous.HasCompleteMemoryLimits ||
		!equality.Semantic.DeepEqual(current.PodSpec.NodeSelector, previous.PodSpec.NodeSelector) ||
		!equality.Semantic.DeepEqual(current.PodSpec.Affinity, previous.PodSpec.Affinity) ||
		current.PodSpec.NodeName != previous.PodSpec.NodeName
}

func snapshotStale(policy *quotav1alpha1.ElasticQuotaPolicy, now time.Time) (bool, time.Duration) {
	if policy.Status.LastCalculationTime == nil {
		return true, 0
	}
	age := now.Sub(policy.Status.LastCalculationTime.Time)
	if age < 0 {
		age = 0
	}
	maximum := policy.Spec.Admission.MaxSnapshotAge.Duration
	return maximum > 0 && age > maximum, age.Round(time.Second)
}

func configuredPool(
	departmentQuota quotav1alpha1.DepartmentQuota,
	poolName string,
) (quotav1alpha1.DepartmentPoolQuota, bool) {
	for _, pool := range departmentQuota.Spec.Pools {
		if pool.Name == poolName {
			return pool, true
		}
	}
	return quotav1alpha1.DepartmentPoolQuota{}, false
}

func departmentPoolStatus(
	departmentQuota quotav1alpha1.DepartmentQuota,
	poolName string,
) (quotav1alpha1.DepartmentPoolQuotaStatus, bool) {
	for _, pool := range departmentQuota.Status.Pools {
		if pool.Name == poolName {
			return pool, true
		}
	}
	return quotav1alpha1.DepartmentPoolQuotaStatus{}, false
}

func quotaExceededMessage(
	department string,
	poolName string,
	budgetName string,
	allocated corev1.ResourceList,
	delta corev1.ResourceList,
	budget corev1.ResourceList,
) string {
	return fmt.Sprintf(
		"department %s exceeds memory %s in resource pool %s: allocatedLimits=%s, limitDelta=%s, %s=%s; reduce replicas or memory limits, or request a higher quota",
		department,
		budgetName,
		poolName,
		memoryString(allocated),
		memoryString(delta),
		budgetName,
		memoryString(budget),
	)
}

func blockingIssue(message string) admissionIssue {
	return admissionIssue{message: message, blockInEnforce: true}
}

func respond(mode quotav1alpha1.AdmissionMode, issues []admissionIssue) admission.Response {
	if len(issues) == 0 {
		return admission.Allowed("memory quota validation passed")
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].message < issues[j].message })
	messages := make([]string, 0, len(issues))
	hasBlockingIssue := false
	for _, issue := range issues {
		messages = append(messages, issue.message)
		hasBlockingIssue = hasBlockingIssue || issue.blockInEnforce
	}
	message := strings.Join(messages, "; ")
	switch mode {
	case quotav1alpha1.AdmissionModeEnforce:
		if hasBlockingIssue {
			return denied(message)
		}
		return admission.Allowed("quota validation allowed by fail-open policy").WithWarnings(warning(message))
	case quotav1alpha1.AdmissionModeWarn:
		return admission.Allowed("quota validation warning").WithWarnings(warning(message))
	default:
		return admission.Allowed("quota observation recorded")
	}
}

func denied(message string) admission.Response {
	return admission.Response{AdmissionResponse: admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Code:    http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: message,
		},
	}}
}

func warning(message string) string {
	if len(message) <= maxAdmissionWarningLength {
		return message
	}
	return message[:maxAdmissionWarningLength-3] + "..."
}

func (h *Handler) internalError(err error, operation string) admission.Response {
	h.log.Error(err, operation)
	return admission.Errored(
		http.StatusInternalServerError,
		errors.New("quota admission state is temporarily unavailable"),
	)
}

func (h *Handler) currentTime() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
}

func memoryValue(resources corev1.ResourceList) int64 {
	memory := resources[corev1.ResourceMemory]
	return memory.Value()
}

func memoryString(resources corev1.ResourceList) string {
	memory, exists := resources[corev1.ResourceMemory]
	if !exists {
		return "0"
	}
	return memory.String()
}

func validatePoolPlacement(
	ctx context.Context,
	reader client.Reader,
	spec corev1.PodSpec,
	pool quotav1alpha1.ResourcePool,
) error {
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.NodeSelector)
	if err != nil {
		return fmt.Errorf("resource pool node selector is invalid")
	}
	if spec.NodeName != "" {
		node, err := getNode(ctx, reader, spec.NodeName)
		if err != nil {
			return fmt.Errorf("assigned node cannot be verified")
		}
		if !selector.Matches(labels.Set(node.Labels)) {
			return fmt.Errorf("assigned node is outside the declared resource pool")
		}
		return nil
	}

	requirements := make(map[string]poolRequirement)
	for key, value := range pool.Spec.NodeSelector.MatchLabels {
		requirements[key] = poolRequirement{allowedValues: map[string]struct{}{value: {}}}
	}
	for _, expression := range pool.Spec.NodeSelector.MatchExpressions {
		switch expression.Operator {
		case metav1.LabelSelectorOpIn:
			allowed := make(map[string]struct{}, len(expression.Values))
			for _, value := range expression.Values {
				allowed[value] = struct{}{}
			}
			requirements[expression.Key] = poolRequirement{allowedValues: allowed}
		case metav1.LabelSelectorOpExists:
			requirements[expression.Key] = poolRequirement{requiresExistence: true}
		default:
			return fmt.Errorf("resource pool selector operator %s is not supported for admission implication", expression.Operator)
		}
	}
	for key, requirement := range requirements {
		if !podConstraintsGuarantee(spec, key, requirement) {
			return fmt.Errorf("hard node constraints do not guarantee resource pool selector key %s", key)
		}
	}
	return nil
}

type poolRequirement struct {
	allowedValues     map[string]struct{}
	requiresExistence bool
}

func podConstraintsGuarantee(spec corev1.PodSpec, key string, required poolRequirement) bool {
	if value, exists := spec.NodeSelector[key]; exists {
		return required.accepts(value)
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !termGuarantees(term, key, required) {
			return false
		}
	}
	return true
}

func termGuarantees(term corev1.NodeSelectorTerm, key string, required poolRequirement) bool {
	for _, expression := range term.MatchExpressions {
		if expression.Key != key {
			continue
		}
		switch expression.Operator {
		case corev1.NodeSelectorOpIn:
			if len(expression.Values) == 0 {
				return false
			}
			for _, value := range expression.Values {
				if !required.accepts(value) {
					return false
				}
			}
			return true
		case corev1.NodeSelectorOpExists, corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
			return required.requiresExistence
		default:
			return false
		}
	}
	return false
}

func (r poolRequirement) accepts(value string) bool {
	if r.requiresExistence {
		return true
	}
	_, exists := r.allowedValues[value]
	return exists
}
