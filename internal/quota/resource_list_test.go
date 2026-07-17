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

package quota

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCloneDoesNotAliasInput(t *testing.T) {
	input := resourceList("1Gi", "")
	cloned := Clone(input)

	cloned[corev1.ResourceMemory] = resource.MustParse("2Gi")

	if got := input[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("Clone mutated its input: got %s", got.String())
	}
}

func TestResourceListArithmetic(t *testing.T) {
	tests := []struct {
		name      string
		operation func(corev1.ResourceList, corev1.ResourceList) corev1.ResourceList
		left      corev1.ResourceList
		right     corev1.ResourceList
		want      corev1.ResourceList
	}{
		{
			name:      "add",
			operation: Add,
			left:      resourceList("2Gi", "1"),
			right:     resourceList("3Gi", "2"),
			want:      resourceList("5Gi", "3"),
		},
		{
			name:      "subtract preserves negative values",
			operation: Subtract,
			left:      resourceList("2Gi", ""),
			right:     resourceList("3Gi", ""),
			want:      resourceList("-1Gi", ""),
		},
		{
			name:      "maximum",
			operation: Max,
			left:      resourceList("2Gi", "3"),
			right:     resourceList("3Gi", "2"),
			want:      resourceList("3Gi", "3"),
		},
		{
			name:      "minimum",
			operation: Min,
			left:      resourceList("2Gi", "3"),
			right:     resourceList("3Gi", "2"),
			want:      resourceList("2Gi", "2"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.operation(test.left, test.right)
			if !Equal(got, test.want) {
				t.Fatalf("unexpected result: got %v, want %v", got, test.want)
			}
		})
	}
}

func TestClampNonNegative(t *testing.T) {
	input := resourceList("-1Gi", "2")
	want := resourceList("0", "2")

	if got := ClampNonNegative(input); !Equal(got, want) {
		t.Fatalf("unexpected result: got %v, want %v", got, want)
	}
}

func TestComparisonTreatsMissingAsZero(t *testing.T) {
	zeroMemory := resourceList("0", "")

	if !Equal(nil, zeroMemory) {
		t.Fatal("nil and an explicit zero quantity should compare equal")
	}
	if !LessThanOrEqual(resourceList("1Gi", ""), resourceList("2Gi", "")) {
		t.Fatal("1Gi should be less than or equal to 2Gi")
	}
	if LessThanOrEqual(resourceList("2Gi", ""), resourceList("1Gi", "")) {
		t.Fatal("2Gi should not be less than or equal to 1Gi")
	}
}

func TestMultiply(t *testing.T) {
	got, err := Multiply(resourceList("5Gi", ""), resource.MustParse("1.2"))
	if err != nil {
		t.Fatalf("Multiply returned an error: %v", err)
	}
	if want := resourceList("6Gi", ""); !Equal(got, want) {
		t.Fatalf("unexpected result: got %v, want %v", got, want)
	}

	if _, err := Multiply(resourceList("5Gi", ""), resource.MustParse("-1")); err == nil {
		t.Fatal("Multiply should reject a negative factor")
	}
}

func resourceList(memory, pods string) corev1.ResourceList {
	resources := corev1.ResourceList{}
	if memory != "" {
		resources[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	if pods != "" {
		resources[corev1.ResourcePods] = resource.MustParse(pods)
	}
	return resources
}
