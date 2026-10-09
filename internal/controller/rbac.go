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

// Package controller contains the quota reconciliation controllers.
package controller

// These markers define the phase-one permission boundary. In particular, the
// manager has no Pod delete or policy/evictions permission.

// +kubebuilder:rbac:groups="",resources=nodes;namespaces;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=quota.kgpp34.io,resources=resourcepools;departmentquotas;elasticquotapolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=quota.kgpp34.io,resources=resourcepools/status;departmentquotas/status;elasticquotapolicies/status,verbs=get;update;patch
