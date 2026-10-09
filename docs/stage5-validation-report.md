# Stage 5 Validation Report

Date: 2026-07-17  
Environment: `kind-dev`, Kubernetes v1.28.0, two simulated heterogeneous node pools, metrics-server available  
Compatibility baseline: Kubernetes and Metrics API clients v0.23.5

## Result

Stage 5 passed. Prometheus and Grafana were not installed in Kind and were not required for controller operation or validation.

## Automated checks

| Check | Result |
| --- | --- |
| `make generate manifests` | Passed |
| `go test ./...` | Passed |
| `go test -race ./...` | Passed |
| `go vet ./...` | Passed |
| `go mod verify` | Passed |
| `git diff --check` | Passed |
| default, samples, Prometheus, and Grafana Kustomize builds | Passed |
| Grafana dashboard JSON validation | Passed |

Unit tests cover Pod sample attribution, terminal and missing sample exclusion, container usage summation, last-good caching after refresh failure, defensive copies, and bounded metric labels. The full race suite completed without a race report.

## Kind smoke test

The controller ran locally against `kind-dev`; its native HTTP metrics endpoint listened on `127.0.0.1:18080`. Three isolated test Pods ran across the simulated ARM64v8/Kylin and AMD64/Kylin pools.

Observed DepartmentQuota status:

```text
stage2-amd64-kylin observedUsage.memory = 400Ki
stage2-arm64v8-kylin observedUsage.memory = 204Ki
```

Matching native metrics:

```text
resource_elastic_quota_department_observed_usage_memory_bytes{department="stage2",pool="stage2-amd64-kylin"} 409600
resource_elastic_quota_department_observed_usage_memory_bytes{department="stage2",pool="stage2-arm64v8-kylin"} 208896
resource_elastic_quota_metrics_api_healthy 1
```

Pool observed usage was higher by design: the pool risk view includes system Pods on matching nodes, while department accounting includes only explicitly managed business Namespaces.

The policy was temporarily switched to an unsupported provider to exercise degradation. `MetricsDegraded=True` with reason `UnsupportedProvider`, while strict status remained unchanged:

```text
AMD allocatedLimits.memory = 320Mi, effectiveQuota.memory = 1Gi
ARM allocatedLimits.memory = 128Mi, effectiveQuota.memory = 1Gi
```

No Pod was evicted. Manager RBAC contains no Pod delete or `pods/eviction` permission. All isolated Namespace, Pod, policy, pool, and department objects were deleted after validation; the generated kgpp34 CRDs were retained.

## Environment note

Kind also contains legacy quota CRDs from an older build. Validation used fully qualified `*.quota.kgpp34.io` resource names to avoid kubectl discovery ambiguity. Those legacy CRDs were outside this stage's scope and were not deleted.
