# Resource Elastic Quota Operations Runbook

## Scope and safety model

The controller enforces aggregate `limits.memory`. CPU and requests are not quota inputs. Metrics API usage is diagnostic only: a metrics-server outage must not disable limits accounting or admission. Automatic Pod eviction remains disabled; this release does not delete Pods or call the Eviction API.

## Health checks without Prometheus

Prometheus and Grafana are optional. The manager exposes controller-runtime and quota metrics over HTTP on port 8080.

```sh
kubectl -n resource-elastic-quota-system port-forward service/resource-elastic-quota-controller-manager-metrics-service 8080:8080
curl --fail --silent http://127.0.0.1:8080/metrics | grep resource_elastic_quota
```

Check the control-plane status independently:

```sh
kubectl get elasticquotapolicy default -o yaml
kubectl get resourcepools
kubectl get departmentquotas
kubectl get --raw /apis/metrics.k8s.io/v1beta1/pods
```

Expected policy conditions are `Ready=True`, `AccountingDegraded=False`, and `MetricsDegraded=False`. `MetricsDegraded=True` affects `ObservedUsage` only; strict admission continues from `AllocatedLimits` and `EffectiveQuota`.

## Important metrics

| Metric | Meaning |
| --- | --- |
| `resource_elastic_quota_snapshot_age_seconds` | Age of the latest successful strict limits snapshot |
| `resource_elastic_quota_metrics_api_healthy` | Latest Metrics API refresh health; 0 does not disable admission |
| `resource_elastic_quota_metrics_sample_age_seconds` | Age of the newest included Pod usage sample |
| `resource_elastic_quota_pool_*_memory_bytes` | Physical capacity, admission budget, limits, and observed use by pool |
| `resource_elastic_quota_department_*_memory_bytes` | Base/max/effective quota, limits, borrow, and observed use by department and pool |
| `resource_elastic_quota_admission_requests_total` | Evaluated admission decisions by mode/result/fixed reason |
| `resource_elastic_quota_reconcile_total` | Successful and failed reconciliations |
| `resource_elastic_quota_reconcile_duration_seconds` | Reconciliation latency histogram |

No metric uses Pod, Namespace, object name, or UID labels.

## Optional Prometheus and Grafana installation

Install the Prometheus Operator CRDs before applying the monitoring resources:

```sh
kubectl apply -k config/prometheus
```

The ServiceMonitor scrapes the existing HTTP metrics Service. The PrometheusRule includes stale snapshot, Metrics API degradation, department quota saturation, pool budget saturation, and repeated reconcile failure alerts.

If Grafana sidecar dashboard discovery is configured for ConfigMaps labeled `grafana_dashboard=1`:

```sh
kubectl create namespace monitoring # only when the namespace does not already exist
kubectl apply -k config/grafana
```

Neither overlay belongs to the default installation, so a cluster without Prometheus or Grafana does not require their CRDs.

## Incident response

### Snapshot stale or reconciliation errors

1. Inspect manager logs, leader election, API connectivity, and `ElasticQuotaPolicy/default` conditions.
2. Check RBAC for Nodes, Pods, Namespaces, quota CRDs, statuses, and `metrics.k8s.io/pods`.
3. Keep admission in `Observe` or `Warn` if strict status freshness cannot be guaranteed.
4. Restart one manager Pod only after confirming the second replica is ready. The next reconcile rebuilds state from Kubernetes objects.

### Metrics API degraded

1. Confirm `kubectl top pods -A` and the raw Metrics API work.
2. Inspect metrics-server logs and APIService availability.
3. Do not relax strict quotas solely because observed usage is missing. The cached last-good observation may remain visible while `MetricsDegraded=True`.

### Department over quota

1. Compare allocated limits, effective quota, and the pool admission budget.
2. Find workload templates with positive limit deltas and reduce replicas or limits.
3. Deletion and non-increasing updates remain allowed for remediation.
4. Existing Pods are not evicted.

### Selector or placement fault

Resolve `SelectorOverlap`, unmatched nodes, unknown pools, or Pod pool-label/affinity mismatches before increasing admission strictness. Ambiguous nodes provide no managed capacity.

## Rollout and rollback

Use this sequence for each business Namespace cohort:

1. `Observe`: establish at least seven days of quota, denial-candidate, and metrics freshness data.
2. `Warn`: expose warnings to platform users and repair templates.
3. `Enforce`: start with a small Namespace cohort, then expand after an incident-free observation window.

Rollback changes `spec.admission.mode` from `Enforce` to `Warn` or `Observe`. Do not remove the webhook as the first response: lowering the mode preserves visibility and remediation behavior. Never enable eviction as part of quota rollout; it is a separate, audited feature gate.

## Release gates

- Generated CRDs/RBAC are current and Kubernetes 1.23-compatible.
- Unit tests, race tests, vet, module verification, and Kustomize builds pass.
- A Kind smoke test sees quota metrics without Prometheus and sees `ObservedUsage` when metrics-server has samples.
- Webhook Observe/Warn/Enforce and deletion/non-increasing remediation tests pass.
- The controller ServiceAccount has no Pod delete or `pods/eviction` permission.
