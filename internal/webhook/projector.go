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
	"fmt"
	"math/big"
	"strconv"

	"kgpp34.com/resource-elastic-quota/internal/accounting"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// PodDepartmentLabel is the business department label required on Pods and templates.
	PodDepartmentLabel = "department"
)

// Projection is the quota-relevant representation of a Pod or workload.
type Projection struct {
	Department              string
	Pool                    string
	PodSpec                 corev1.PodSpec
	Limits                  corev1.ResourceList
	HasResourceProjection   bool
	HasCompleteMemoryLimits bool
}

// Projector converts supported admission objects into quota projections.
type Projector interface {
	Project(context.Context, schema.GroupVersionKind, runtime.RawExtension) (Projection, error)
}

// BuiltinProjector supports the Kubernetes built-in workload types covered by phase four.
type BuiltinProjector struct {
	decoder *admission.Decoder
	reader  client.Reader
}

// NewBuiltinProjector constructs a projector backed by the manager cache.
func NewBuiltinProjector(decoder *admission.Decoder, reader client.Reader) *BuiltinProjector {
	return &BuiltinProjector{decoder: decoder, reader: reader}
}

// Project decodes and projects one supported object.
func (p *BuiltinProjector) Project(
	ctx context.Context,
	kind schema.GroupVersionKind,
	raw runtime.RawExtension,
) (Projection, error) {
	switch kind.GroupKind() {
	case corev1.SchemeGroupVersion.WithKind("Pod").GroupKind():
		pod := &corev1.Pod{}
		if err := p.decode(raw, pod); err != nil {
			return Projection{}, err
		}
		return projectPod(pod.Labels, pod.Spec, 1)
	case appsv1.SchemeGroupVersion.WithKind("Deployment").GroupKind():
		object := &appsv1.Deployment{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		return projectPod(object.Spec.Template.Labels, object.Spec.Template.Spec, replicas(object.Spec.Replicas))
	case appsv1.SchemeGroupVersion.WithKind("StatefulSet").GroupKind():
		object := &appsv1.StatefulSet{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		return projectPod(object.Spec.Template.Labels, object.Spec.Template.Spec, replicas(object.Spec.Replicas))
	case appsv1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind():
		object := &appsv1.ReplicaSet{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		return projectPod(object.Spec.Template.Labels, object.Spec.Template.Spec, replicas(object.Spec.Replicas))
	case appsv1.SchemeGroupVersion.WithKind("DaemonSet").GroupKind():
		object := &appsv1.DaemonSet{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		count, err := p.daemonSetNodeCount(ctx, object.Spec.Template.Spec)
		if err != nil {
			return Projection{}, fmt.Errorf("count daemonset target nodes: %w", err)
		}
		return projectPod(object.Spec.Template.Labels, object.Spec.Template.Spec, count)
	case batchv1.SchemeGroupVersion.WithKind("Job").GroupKind():
		object := &batchv1.Job{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		return projectPod(object.Spec.Template.Labels, object.Spec.Template.Spec, parallelism(object.Spec.Parallelism))
	case batchv1.SchemeGroupVersion.WithKind("CronJob").GroupKind():
		object := &batchv1.CronJob{}
		if err := p.decode(raw, object); err != nil {
			return Projection{}, err
		}
		projection, err := projectPod(
			object.Spec.JobTemplate.Spec.Template.Labels,
			object.Spec.JobTemplate.Spec.Template.Spec,
			parallelism(object.Spec.JobTemplate.Spec.Parallelism),
		)
		if err != nil {
			return Projection{}, err
		}
		if object.Spec.ConcurrencyPolicy == batchv1.AllowConcurrent {
			projection.Limits = corev1.ResourceList{}
			projection.HasResourceProjection = false
		}
		return projection, nil
	default:
		return Projection{}, fmt.Errorf("unsupported admission kind %s", kind.GroupKind().String())
	}
}

func (p *BuiltinProjector) decode(raw runtime.RawExtension, object runtime.Object) error {
	if p.decoder == nil {
		return fmt.Errorf("admission decoder is not initialized")
	}
	if err := p.decoder.DecodeRaw(raw, object); err != nil {
		return fmt.Errorf("decode admission object: %w", err)
	}
	return nil
}

func (p *BuiltinProjector) daemonSetNodeCount(ctx context.Context, spec corev1.PodSpec) (int64, error) {
	nodes := &corev1.NodeList{}
	if err := p.reader.List(ctx, nodes); err != nil {
		return 0, err
	}
	var count int64
	for i := range nodes.Items {
		node := nodes.Items[i]
		if !nodeReadyAndSchedulable(node) || !podMatchesNode(spec, node) {
			continue
		}
		count++
	}
	return count, nil
}

func projectPod(labels map[string]string, spec corev1.PodSpec, count int64) (Projection, error) {
	limits, err := multiplyMemory(accounting.PodLimits(corev1.Pod{Spec: spec}), count)
	if err != nil {
		return Projection{}, err
	}
	return Projection{
		Department:              labels[PodDepartmentLabel],
		Pool:                    labels[accounting.PodResourcePoolLabel],
		PodSpec:                 *spec.DeepCopy(),
		Limits:                  limits,
		HasResourceProjection:   true,
		HasCompleteMemoryLimits: hasCompleteMemoryLimits(spec),
	}, nil
}

func hasCompleteMemoryLimits(spec corev1.PodSpec) bool {
	for i := range spec.Containers {
		memory, exists := spec.Containers[i].Resources.Limits[corev1.ResourceMemory]
		if !exists || memory.Sign() <= 0 {
			return false
		}
	}
	for i := range spec.InitContainers {
		memory, exists := spec.InitContainers[i].Resources.Limits[corev1.ResourceMemory]
		if !exists || memory.Sign() <= 0 {
			return false
		}
	}
	return len(spec.Containers) > 0
}

func multiplyMemory(resources corev1.ResourceList, count int64) (corev1.ResourceList, error) {
	if count < 0 {
		return nil, fmt.Errorf("replica count must be non-negative")
	}
	memory := resources[corev1.ResourceMemory]
	if memory.Sign() < 0 {
		return nil, fmt.Errorf("memory limit must be non-negative")
	}
	product := new(big.Int).Mul(big.NewInt(memory.Value()), big.NewInt(count))
	if !product.IsInt64() {
		return nil, fmt.Errorf("projected memory limit exceeds supported range")
	}
	return memoryList(product.Int64()), nil
}

func replicas(value *int32) int64 {
	if value == nil {
		return 1
	}
	return int64(*value)
}

func parallelism(value *int32) int64 {
	if value == nil {
		return 1
	}
	return int64(*value)
}

func nodeReadyAndSchedulable(node corev1.Node) bool {
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

func podMatchesNode(spec corev1.PodSpec, node corev1.Node) bool {
	if spec.NodeName != "" && spec.NodeName != node.Name {
		return false
	}
	for key, value := range spec.NodeSelector {
		if node.Labels[key] != value {
			return false
		}
	}
	required := spec.Affinity
	if required == nil || required.NodeAffinity == nil ||
		required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	terms := required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i := range terms {
		if nodeMatchesTerm(node, terms[i]) {
			return true
		}
	}
	return false
}

func nodeMatchesTerm(node corev1.Node, term corev1.NodeSelectorTerm) bool {
	for i := range term.MatchExpressions {
		if !nodeMatchesRequirement(node.Labels, term.MatchExpressions[i]) {
			return false
		}
	}
	for i := range term.MatchFields {
		requirement := term.MatchFields[i]
		if requirement.Key != "metadata.name" || !valueMatchesRequirement(node.Name, true, requirement) {
			return false
		}
	}
	return true
}

func nodeMatchesRequirement(values map[string]string, requirement corev1.NodeSelectorRequirement) bool {
	value, exists := values[requirement.Key]
	return valueMatchesRequirement(value, exists, requirement)
}

func valueMatchesRequirement(value string, exists bool, requirement corev1.NodeSelectorRequirement) bool {
	switch requirement.Operator {
	case corev1.NodeSelectorOpIn:
		return exists && contains(requirement.Values, value)
	case corev1.NodeSelectorOpNotIn:
		return !exists || !contains(requirement.Values, value)
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if !exists || len(requirement.Values) != 1 {
			return false
		}
		actual, actualErr := strconv.ParseInt(value, 10, 64)
		expected, expectedErr := strconv.ParseInt(requirement.Values[0], 10, 64)
		if actualErr != nil || expectedErr != nil {
			return false
		}
		if requirement.Operator == corev1.NodeSelectorOpGt {
			return actual > expected
		}
		return actual < expected
	default:
		return false
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func memoryList(bytes int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceMemory: *resource.NewQuantity(bytes, resource.BinarySI),
	}
}

func getNode(ctx context.Context, reader client.Reader, name string) (corev1.Node, error) {
	node := corev1.Node{}
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return corev1.Node{}, err
	}
	return node, nil
}
