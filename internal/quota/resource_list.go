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

// Package quota provides resource accounting primitives shared by controllers
// and admission handlers.
package quota

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Clone returns a deep copy of resources. A nil input becomes an empty list.
func Clone(resources corev1.ResourceList) corev1.ResourceList {
	result := make(corev1.ResourceList, len(resources))
	for name, quantity := range resources {
		result[name] = quantity.DeepCopy()
	}
	return result
}

// Add returns the element-wise sum of left and right.
func Add(left, right corev1.ResourceList) corev1.ResourceList {
	return combine(left, right, func(leftQuantity, rightQuantity resource.Quantity) resource.Quantity {
		leftQuantity.Add(rightQuantity)
		return leftQuantity
	})
}

// Subtract returns left minus right. Negative quantities are preserved.
func Subtract(left, right corev1.ResourceList) corev1.ResourceList {
	return combine(left, right, func(leftQuantity, rightQuantity resource.Quantity) resource.Quantity {
		leftQuantity.Sub(rightQuantity)
		return leftQuantity
	})
}

// Max returns the element-wise maximum of left and right.
func Max(left, right corev1.ResourceList) corev1.ResourceList {
	return combine(left, right, func(leftQuantity, rightQuantity resource.Quantity) resource.Quantity {
		if leftQuantity.Cmp(rightQuantity) >= 0 {
			return leftQuantity
		}
		return rightQuantity
	})
}

// Min returns the element-wise minimum of left and right.
func Min(left, right corev1.ResourceList) corev1.ResourceList {
	return combine(left, right, func(leftQuantity, rightQuantity resource.Quantity) resource.Quantity {
		if leftQuantity.Cmp(rightQuantity) <= 0 {
			return leftQuantity
		}
		return rightQuantity
	})
}

// ClampNonNegative replaces negative quantities with zero.
func ClampNonNegative(resources corev1.ResourceList) corev1.ResourceList {
	result := Clone(resources)
	for name, quantity := range result {
		if quantity.Sign() < 0 {
			result[name] = *resource.NewQuantity(0, quantity.Format)
		}
	}
	return result
}

// LessThanOrEqual reports whether every quantity in left is at most right.
// Missing resources are treated as zero.
func LessThanOrEqual(left, right corev1.ResourceList) bool {
	for name := range resourceNames(left, right) {
		leftQuantity := left[name]
		rightQuantity := right[name]
		if leftQuantity.Cmp(rightQuantity) > 0 {
			return false
		}
	}
	return true
}

// Equal reports whether both resource lists represent the same quantities.
// Missing resources and explicit zero quantities are equivalent.
func Equal(left, right corev1.ResourceList) bool {
	return LessThanOrEqual(left, right) && LessThanOrEqual(right, left)
}

// Multiply scales every quantity by a non-negative decimal factor.
func Multiply(resources corev1.ResourceList, factor resource.Quantity) (corev1.ResourceList, error) {
	if factor.Sign() < 0 {
		return nil, fmt.Errorf("resource multiplier must be non-negative: %s", factor.String())
	}

	result := make(corev1.ResourceList, len(resources))
	for name, quantity := range resources {
		product := quantity.AsDec()
		product.Mul(product, factor.AsDec())
		result[name] = *resource.NewDecimalQuantity(*product, quantity.Format)
	}
	return result, nil
}

func combine(
	left, right corev1.ResourceList,
	operation func(resource.Quantity, resource.Quantity) resource.Quantity,
) corev1.ResourceList {
	result := make(corev1.ResourceList, len(resourceNames(left, right)))
	for name := range resourceNames(left, right) {
		result[name] = operation(left[name].DeepCopy(), right[name].DeepCopy())
	}
	return result
}

func resourceNames(lists ...corev1.ResourceList) map[corev1.ResourceName]struct{} {
	names := make(map[corev1.ResourceName]struct{})
	for _, resources := range lists {
		for name := range resources {
			names[name] = struct{}{}
		}
	}
	return names
}
