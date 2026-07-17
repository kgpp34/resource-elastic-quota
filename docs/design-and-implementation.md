# Elastic Resource Quota Controller: Design and Implementation Plan

Copyright kgpp34 2026.

## 1. Purpose

This document defines the engineering, testing, and acceptance baseline for a Kubebuilder-based Kubernetes elastic resource quota controller.

The controller must:

- support Kubernetes 1.23 and later;
- support heterogeneous pools such as ARM64/Kylin and AMD64/Kylin without hard-coded node-type enums;
- account for each department's Pod `limits.memory` independently in every resource pool;
- maintain a historical base quota and an absolute maximum quota per department;
- lend unused quota while enforcing a pool-wide oversubscription boundary;
- reject resource-increasing workload requests after department or pool exhaustion; and
- define consistency, failure, and safety behavior explicitly.

Version 1 is memory-only. Pod requests, CPU requests, and CPU limits do not participate in accounting, allocation, or admission. Actual memory usage is an operational signal rather than the strict admission ledger. Internal arithmetic still uses `corev1.ResourceList` to preserve future extension points.

## 2. Requirements and Boundaries

### 2.1 Trusted department identity

Business Pods carry a department label:

```yaml
metadata:
  labels:
    department: department-a
```

This is not a trusted identity because a user can change it. The platform binds each managed Namespace to one department with administrator-controlled labels:

```yaml
metadata:
  labels:
    quota.kgpp34.io/enforced: "true"
    quota.kgpp34.io/department: department-a
```

The webhook requires the Pod or workload-template `department` label to equal the Namespace's `quota.kgpp34.io/department` value.

Management is explicit opt-in rather than a system Namespace denylist:

- Only Namespaces with both `quota.kgpp34.io/enforced=true` and a department binding participate.
- Namespaces such as `kube-system` and `koordinator-system` remain unmanaged unless explicitly opted in.
- A department label without `enforced=true` does not enable quota, supporting gradual rollout.
- An enforced Namespace without a department is a platform configuration error. The controller reports it and Enforce mode rejects new business workloads.
- System Pods are not charged to a department, but scheduled system Pods still count against their ResourcePool's aggregate allocation because they consume the same nodes.

### 2.2 Resource definitions

| Value | Source | Purpose | Admission input |
| --- | --- | --- | --- |
| Allocated memory limits | Pod specs | Department and pool ledger | Yes |
| Observed memory usage | metrics-server or monitoring | Risk, utilization, alerts | No |
| Effective quota | Controller calculation | Current department allowance | Yes |

The strict ledger is aggregate container `limits.memory`. The scheduler continues to place Pods using requests independently. Observed usage may guide lending, risk alerts, and capacity planning, but it cannot replace declared limits as the admission basis. If metrics-server is unavailable, limits accounting and admission must continue.

### 2.3 Meaning of rejection

Exceeding quota must not block ConfigMaps, Secrets, Events, deletion, or remediation:

> Reject creation or expansion of Pods and workloads that increase a department's aggregate `limits.memory`; always allow deletion, scale-down, and lower memory limits. Request-only changes remain outside this controller's scope.

## 3. Corrections to the Earlier Proposal

The earlier research-environment proposal is background, not an implementation specification:

1. Instant metrics-server usage is not allocated capacity and is unsuitable for strict admission.
2. Native ResourceQuota aggregates by Namespace and cannot separate department labels or node types inside one Namespace.
3. Applying a formula independently to every department can allocate the same shared capacity multiple times.
4. Lowering ResourceQuota does not evict existing Pods.
5. Kubernetes 1.23 cannot generally change running Pod resources without replacement.
6. Converting workloads to BestEffort destroys the limits ledger and increases OOM risk.
7. Pod priority, PDB, and node-pressure eviction have separate semantics.
8. Cache-based admission requires an explicit concurrent-consistency model.

## 4. Architecture

```text
Node / Pod / Namespace / quota CR events
                    |
                    v
       ElasticQuotaPolicy Controller
       +-- classify node capacity by pool
       +-- aggregate Pod limits.memory
       +-- allocate and reclaim lent quota
       +-- optionally collect observed usage
       +-- publish CR status/admission snapshot
                    |
                    v
             Validating Webhook
       +-- verify department identity
       +-- verify the target resource pool
       +-- project the positive memory delta
       +-- allow, warn, or reject
```

Version 1 has three cluster-scoped CRDs:

- `ResourcePool`: a node pool and its capacity policy.
- `DepartmentQuota`: one department's base and maximum quota in each pool.
- `ElasticQuotaPolicy`: cluster-wide allocation, pressure, metrics, eviction, and admission policy.

Reconciliation is event-driven with periodic full recalculation for eventual consistency.

## 5. API Design

The API group is `quota.kgpp34.io/v1alpha1`. Ratios use string-form `resource.Quantity` values such as `"1.5"` to avoid floating-point serialization and precision problems.

### 5.1 ResourcePool

Each cluster-scoped `ResourcePool` represents an independently accounted node pool.

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: ResourcePool
metadata:
  name: arm64v8-kylin
spec:
  nodeSelector:
    matchLabels:
      nodetype.cks.io/arch: arm64v8
      nodetype.cks.io/os: kylin
  capacityPolicy:
    reserve:
      memory: 16Gi
    admissionRatio:
      memory: "1.5"
    entitlementRatio:
      memory: "2.0"
status:
  readyNodes: 10
  allocatable:
    memory: 640Gi
  admissionBudget:
    memory: 936Gi
  configuredEntitlementBudget:
    memory: 1248Gi
  effectiveEntitlementBudget:
    memory: 1248Gi
  allocatedLimits:
    memory: 520Gi
```

Adding a node type requires a new ResourcePool, not a code change.

Capacity rules:

- Sum `Node.status.allocatable`, not `status.capacity`.
- Only Ready and schedulable nodes provide capacity for new lending.
- Cordoned nodes stop providing new capacity, while their running Pods remain allocated.
- Pods on NotReady or Unknown nodes remain allocated until deletion.
- A node must match exactly one pool.
- A multi-pool match produces `SelectorOverlap=True`; the node is excluded from new capacity.
- An unmatched node produces metrics and Events but no managed capacity.

### 5.2 DepartmentQuota

One cluster-scoped object represents one department across multiple pools.

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: DepartmentQuota
metadata:
  name: department-a
spec:
  department: department-a
  evictionPolicy: Deny
  namespaceSelector:
    matchLabels:
      quota.kgpp34.io/enforced: "true"
      quota.kgpp34.io/department: department-a
  pools:
    - name: arm64v8-kylin
      baseQuota:
        memory: 48Gi
      maxQuota:
        memory: 128Gi
      weight: 100
      targetUtilizationPercent: 80
      growthHeadroomPercent: 20
      reclaimAfter: 10m
```

- `baseQuota.memory` is the historical lower bound for normal dynamic reclamation.
- `maxQuota.memory` is the absolute department limit after borrowing.
- `effectiveQuota` is the controller-assigned current limit between base and maximum.
- `evictionPolicy` gates severe-pressure eviction only; it does not change quota accounting.
- `weight` controls weighted fair sharing under scarcity.
- `targetUtilizationPercent` grants capacity before the department reaches its limit.
- `growthHeadroomPercent` adds a growth margin.
- `reclaimAfter` requires sustained low demand before reclaim.

Base quota is a historical entitlement, not a physical reservation. The pool admission budget remains authoritative when all departments attempt to use their entitlements simultaneously.

Configuration rejects base greater than maximum, negative or disabled resources, duplicate or unknown pools, non-positive weights, invalid utilization percentages, and Namespaces matching multiple departments.

### 5.3 ElasticQuotaPolicy

Version 1 permits one cluster-scoped object named `default`.

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: ElasticQuotaPolicy
metadata:
  name: default
spec:
  resources: [memory]
  allocation:
    maxIncreasePercent: 20
    minIncrease:
      memory: 1Gi
    maxDecreasePercent: 10
    cooldown: 2m
    fullResync: 5m
  pressure:
    blockBorrowingOnNodeMemoryPressure: true
    highWatermarkPercent: 85
    severeWatermarkPercent: 95
    recoveryWindow: 5m
  metrics:
    enabled: true
    provider: MetricsAPI
    collectionInterval: 30s
    maxSampleAge: 2m
  eviction:
    enabled: false
    trigger: SevereNodePressure
    sustainedFor: 5m
    maxPodsPerCycle: 1
    respectPDB: true
  admission:
    mode: Observe
    maxSnapshotAge: 10s
    failClosedOnStaleSnapshot: false
    failClosedOnUnknownPool: false
```

Admission modes are `Observe` (record only), `Warn` (allow with warnings and Events), and `Enforce` (reject invalid or over-budget positive changes).

The first observed-usage provider is `metrics.k8s.io/v1beta1`. Prometheus is a later optional provider behind a small interface. Automated eviction defaults to disabled and requires both global `eviction.enabled=true` and department `evictionPolicy=Allow`. If enabled, it must use `policy/v1` Eviction, respect PDBs, be rate limited, and target only borrowing business workloads during sustained severe pressure.

## 6. Department and ResourcePool Accounting

`department x ResourcePool` is a two-dimensional ledger, not a Namespace-splitting requirement. One department may run ARM and AMD workloads in the same Namespace, but capacity in one pool cannot satisfy workloads constrained to another.

Every managed Pod template declares its pool:

```yaml
metadata:
  labels:
    department: department-a
    quota.kgpp34.io/resource-pool: arm64v8-kylin
```

The webhook verifies that the pool exists and is Ready, hard node selection proves placement in it, preferred affinity alone is insufficient, and the scheduled Pod is finally charged to the actual Node's pool. A declared/actual mismatch raises `PoolMismatch` and is charged to the actual pool.

Version 1 requires an explicit pool label and does not guess an architecture. A later mutating webhook may inject scheduling constraints from an explicit pool label, but must not choose a pool for the user.

## 7. Pod Resource Calculation

Pending, Running, Unknown, and terminating-but-not-deleted Pods count. Succeeded and Failed Pods do not. This prevents temporary double spending during rolling updates and rapid replacement.

```text
regularContainers = sum(memory limits of regular containers)
initContainers    = max(memory limit among init containers)
podEffective      = max(regularContainers, initContainers) + podOverhead
```

Only `limits.memory` participates. Requests, CPU, ephemeral storage, huge pages, GPUs, and other extended resources are excluded. Every regular and init container in a managed workload must declare a positive memory limit.

## 8. Real-Time Accounting

The controller watches Pods, Nodes, Namespaces, ResourcePools, DepartmentQuotas, and ElasticQuotaPolicy. Useful Pod indexes are department, resource pool, node name, and Namespace.

`ElasticQuotaPolicyReconciler` is the cluster-wide planner:

1. map relevant events to `ElasticQuotaPolicy/default`;
2. let the workqueue coalesce duplicates;
3. read cluster state from the informer cache;
4. build an immutable calculation snapshot;
5. calculate all allocations and effective quotas;
6. patch changed status fields; and
7. atomically publish the admission snapshot.

Periodic full reconciliation repairs missed watches and transient failures. Background work follows Manager context. Status is patched only when values or Conditions change, and quantities are compared numerically rather than by string form.

## 9. Dynamic Oversubscription Algorithm

### 9.1 Pool budgets

```text
physical = sum(Ready and schedulable node allocatable) - reserve
admissionBudget = physical * admissionRatio
configuredEntitlementBudget = physical * entitlementRatio
```

`admissionBudget` is the hard aggregate `limits.memory` boundary. `entitlementBudget` is the nominal total distributable to departments. Nominal entitlements may exceed physical capacity, but admitted memory limits may not exceed the admission budget.

Initial values are admission ratio 1.5 and entitlement ratio 2.0. They must be calibrated in Observe and Warn modes using working set, OOM, MemoryPressure, eviction, and Pending Pod data.

### 9.2 Historical base quotas

```text
sumBaseQuota = sum(all department baseQuota.memory values)
effectiveEntitlementBudget = max(configuredEntitlementBudget, sumBaseQuota)
shared = max(0, effectiveEntitlementBudget - sumBaseQuota)
```

If base quotas exceed capacity, set `BaseQuotaOversubscribed=True`, grant no shared quota until entitlement capacity exceeds the base sum, and continue enforcing the actual pool admission budget. Existing Pods are not deleted.

### 9.3 Department desired quota

```text
utilizationTarget = allocatedMemoryLimits / targetUtilization
growthTarget = allocatedMemoryLimits * (1 + growthHeadroomPercent)
desired = min(max(baseQuota, utilizationTarget, growthTarget), maxQuota)
```

### 9.4 Weighted fair allocation

Assign every base quota, calculate each extra demand, fully satisfy demand if it fits, otherwise distribute the shared pool with weighted max-min fairness. Departments at maximum quota leave the next distribution round.

Required invariants:

```text
baseQuota <= effectiveQuota <= maxQuota
sum(effectiveQuota) <= effectiveEntitlementBudget
effectiveQuota >= allocated, unless explicitly OverQuota
```

Growth is immediate above target but step-limited. Reclaim requires sustained low allocation and usage, cooldown, and a maximum decrease step, and never drops below base or fair share. Node MemoryPressure blocks borrowing and positive admission. Recovery waits for `recoveryWindow`. Eviction is never an ordinary reclaim mechanism.

## 10. Admission Webhook

### 10.1 Scope and extension

The validating webhook covers Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, and CronJobs. Workload checks provide early feedback; Pod admission is the authoritative final boundary for current and future controllers.

Projection is isolated behind a small `Projector` interface. Built-in projectors are registered in Version 1. A custom CRD can add a projector later; until then its resulting Pods remain protected. Generic reflection is not used to guess arbitrary custom-resource replica semantics.

### 10.2 Admission flow

For CREATE:

1. identify a managed Namespace and trusted department;
2. validate the template department label;
3. validate pool label and hard scheduling constraints;
4. require positive memory limits on every regular and init container;
5. project added memory, ignoring requests and CPU;
6. check effective quota and maximum quota;
7. check ResourcePool admission budget; and
8. apply Observe, Warn, or Enforce behavior.

For UPDATE:

```text
delta = max(newProjectedMemory - oldProjectedMemory, 0)
```

DELETE, scale-down, lower memory limits, status updates, and request/CPU-only changes are always allowed by this controller.

### 10.3 Workload projections

```text
Deployment/StatefulSet/ReplicaSet = replicas * podTemplateMemory
Job = parallelism * podTemplateMemory
DaemonSet = matching target-node count * podTemplateMemory
```

CronJob estimation considers concurrency policy where possible. If no reliable upper bound exists, validate structure at workload admission and defer resources to Pod admission. Workload prechecks are not reservations, so child Pods may still be rejected after intervening requests consume capacity.

### 10.4 Configuration and safety

```yaml
failurePolicy: Fail
sideEffects: None
timeoutSeconds: 2
matchPolicy: Equivalent
admissionReviewVersions: [v1]
```

- Use administrator-controlled Namespace labels in `namespaceSelector`.
- Do not use a bypassable workload `objectSelector` based on department labels.
- Read only informer cache data and immutable snapshots; never call monitoring systems on the request path.
- Fail closed for positive deltas in Enforce mode when configured for missing/stale snapshots or unknown departments/pools.
- Run at least two replicas with a PDB and cross-node anti-affinity.
- Exclude the controller Namespace from quota enforcement.
- Restrict Namespace bindings, quota CRs, and webhook configuration to platform administrators.
- Use cert-manager for serving certificates and CA injection.

Rejections include department, pool, allocated amount, delta, effective quota, applicable maximum/budget, and remediation without exposing internal errors.

## 11. Consistency Model

Cache-based label aggregation can permit a small overshoot when concurrent admissions read the same old snapshot. Admission must not write status as a reservation because failures, retries, and replicas can leak or duplicate reservations.

Native ResourceQuota gives apiserver-level consistency only per Namespace. If zero concurrent overshoot becomes mandatory, the alternative is `one department x one ResourcePool = one Namespace`, with platform-managed native ResourceQuota. This changes the current Namespace model and is not selected for Version 1. The current design measures bounded overshoot through concurrency tests.

## 12. Failure and Boundary Behavior

| Scenario | Behavior |
| --- | --- |
| metrics-server unavailable | Set `MetricsDegraded`; limits admission continues |
| unmanaged system Namespace | Skip department accounting and business admission |
| pool has no Ready node | Stop growth; retain existing allocation |
| node becomes NotReady | Remove growth capacity; retain its Pod allocations |
| base sum exceeds capacity | Set `BaseQuotaOversubscribed`; stop lending and enforce admission budget |
| allocated exceeds effective quota | Set `OverQuota`; allow remediation and reject positive deltas |
| department configuration removed | Set `OrphanedDepartment`; reject growth in Enforce mode |
| node matches multiple pools | Set `SelectorOverlap`; exclude new capacity |
| managed Pod lacks pool label | Reject |
| only preferred affinity exists | Reject |
| snapshot missing or stale | Fail closed for positive deltas in Enforce mode according to policy |
| leader changes | Recalculate fully from cached objects |
| status conflict | Patch with retry; never overwrite spec |

## 13. Observability

Recommended metrics:

```text
elastic_quota_pool_allocatable{pool,resource}
elastic_quota_pool_admission_budget{pool,resource}
elastic_quota_pool_allocated{pool,resource,type}
elastic_quota_department_base_quota{department,pool,resource,type}
elastic_quota_department_effective{department,pool,resource,type}
elastic_quota_department_allocated{department,pool,resource,type}
elastic_quota_department_observed_usage{department,pool,resource}
elastic_quota_department_borrowed{department,pool,resource}
elastic_quota_admission_requests_total{mode,result,resource,reason}
elastic_quota_reconcile_duration_seconds
elastic_quota_reconcile_errors_total{reason}
elastic_quota_snapshot_age_seconds
elastic_quota_pool_selector_conflicts_total
```

Pod names and UIDs must not be metric labels. ResourcePool Conditions include Ready, SelectorOverlap, NoReadyNodes, BaseQuotaOversubscribed, AdmissionOverCapacity, and MemoryPressure. DepartmentQuota Conditions include Ready, OverQuota, OrphanedNamespace, UnknownResourcePool, PoolMismatch, and MetricsDegraded. Policy Conditions include Ready, SnapshotStale, ConfigurationInvalid, and MetricsDegraded. Relevant Events are rate limited.

## 14. Kubernetes 1.23 Compatibility

The baseline uses controller-runtime `v0.11.2`, Kubernetes libraries `v0.23.x`, controller-gen `v0.8.x`, `apiextensions.k8s.io/v1`, `admissionregistration.k8s.io/v1`, `policy/v1`, and structural CRD schemas. It avoids ValidatingAdmissionPolicy, CEL match conditions, Pod-level resources, and native sidecar semantics. Supporting 1.23 is a compatibility requirement, not a recommendation to operate an upstream version that no longer receives security maintenance.

## 15. Go Project Design

```text
api/v1alpha1/       CRD types and generated code
cmd/                manager entry point
internal/accounting Pod/node classification and aggregation
internal/allocation dynamic quota planner
internal/controller reconciliation and status publication
internal/quota      pure ResourceList arithmetic
internal/webhook    admission handler and workload projectors
config/             CRDs, RBAC, manager, certs, and webhook manifests
test/e2e/           integration and Kind scenarios
```

Keep arithmetic and allocation pure, avoid generic helper packages, define small interfaces at consumers, let Manager own lifecycles, share immutable snapshots, and deep-copy quantities and ResourceLists correctly.

## 16. RBAC and Security

The controller needs read access to Nodes, Pods, and Namespaces; CRD reads and status updates; Event creation; and optional metrics reads. Native ResourceQuota access belongs only to the optional strong-consistency mode. `pods/eviction` permission must exist only when the separately controlled eviction feature is built and enabled.

With eviction disabled, it must not delete business Pods, mutate workloads, read Secret contents other than mounted serving certificates, or require cluster-admin wildcards. Department users cannot change Namespace bindings, quota CRs, platform ResourceQuotas, or ValidatingWebhookConfiguration.

## 17. Test Plan

Unit tests cover Pod memory calculation, lifecycle, node classification, quota targets, weighted fairness, memory-only admission, capacity shrink, quantities, and overflow. Integration tests cover schemas, workload admission, all operations and modes, stale snapshots, status conflicts, and restart. E2E covers Kubernetes 1.23 plus a newer release, simulated ARM/AMD labels, lending/reclaim, rejection/remediation, node failures, metrics failure, eviction controls, and webhook rollout.

Stability checks include `go test -race ./...`, concurrent overshoot measurement, webhook P99 latency, large-cluster reconciliation, informer reconnects, apiserver outages, and leader changes.

## 18. Implementation Stages and Acceptance

- **Stage 0 — Decisions:** freeze identity, base quota, eviction, metrics, initial quota, and rollout inputs.
- **Stage 1 — Project and CRDs:** deliver the Kubebuilder project, three CRDs, samples, RBAC, generated code, 1.23 dependencies, and ResourceList arithmetic.
- **Stage 2 — Accounting:** deliver node classification, Pod aggregation, status, event-driven reconciliation, and periodic rebuild.
- **Stage 3 — Allocation:** deliver two-layer budgets, weighted fairness, cooldown, headroom, growth/reclaim steps, and pressure behavior with invariant tests.
- **Stage 4 — Webhook:** deliver final Pod admission, early workload checks, identity/pool/placement validation, modes, TLS, and HA. Positive over-budget changes are rejected; remediation and request/CPU-only changes remain allowed.
- **Stage 5 — Observability and rollout:** deliver metrics, alerts, dashboard, runbook, E2E/concurrency reports, and audited severe-pressure eviction controls. Roll out through Observe, Warn, limited Enforce, then broad Enforce.

## 19. Explicit Version 1 Exclusions

- No ordinary quota-driven Pod eviction.
- No automatic reduction of running Pod requests or limits.
- No conversion to BestEffort.
- No custom scheduler plugin.
- No cross-cluster quota.
- No predictive or machine-learning allocation.
- No generic projection of arbitrary custom resources.

## 20. Confirmed Decisions and Remaining Inputs

Confirmed decisions:

1. A managed business Namespace belongs to exactly one department.
2. Only explicitly enforced Namespaces are managed.
3. Namespaces are not split by pool in Version 1.
4. Base quota is a historical non-reclaimable entitlement, not a physical guarantee.
5. Version 1 accounts and limits only `limits.memory`.
6. Automated eviction defaults to disabled and is restricted to sustained severe pressure if enabled.
7. Final Pod admission protects Pods from future custom controllers; workload projectors remain extensible.
8. MetricsAPI is the first usage provider; Prometheus is optional later.
9. Local development uses the existing Kind cluster and metrics-server without requiring Prometheus.

Deployment inputs still required are initial department base/maximum quotas, approval of the initial 1.5/2.0 ratios, initial Observe-mode Namespaces, and departments allowed to opt into severe-pressure eviction.

## 21. References

- [Kubernetes Resource Quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/)
- [Resource Management for Pods and Containers](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/)
- [Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)
- [Kubernetes 1.23 API Reference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.23/)
- [Deprecated API Migration Guide](https://kubernetes.io/docs/reference/using-api/deprecation-guide/)
- [controller-runtime compatibility](https://github.com/kubernetes-sigs/controller-runtime)
- [Kubebuilder 3.3.0](https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v3.3.0)
