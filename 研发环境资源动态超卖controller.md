
## 核心设计思路

传统 `ResourceQuota` 是静态分配，超卖方案的本质是：**按实际使用量动态调整各租户的真实可用额度**，在保证 SLO 的前提下，让集群整体利用率突破 100% 分配线。

主要分三层：

1. **TenantProfile CRD** — 描述租户的优先级、超卖系数、QoS 等级
2. **OvercommitPolicy CRD** — 定义超卖策略（全局 + 租户级）
3. **Operator 控制器** — 监控实际用量，动态回收空闲配额并再分配

![[Pasted image 20260407134524.png]]


![[Pasted image 20260407134733.png]]


## CRD 设计

### TenantProfile — 租户画像

```yaml
apiVersion: overcommit.io/v1alpha1
kind: TenantProfile
metadata:
  name: tenant-a
spec:
  namespace: ns-tenant-a
  tier: platinum                 # platinum / gold / silver
  priorityClass: high-priority
  
  # 基准配额（静态兜底）
  baseQuota:
    cpu: "8"
    memory: "32Gi"
  
  # 超卖系数：允许申请 = base * overcommitRatio
  overcommitRatio:
    cpu: 1.0      # platinum 不超卖 CPU
    memory: 1.5   # 内存允许 1.5 倍
  
  # 弹性上限（集群有空闲时最多能用多少）
  burstLimit:
    cpu: "12"
    memory: "48Gi"
  
  # QoS 保障
  evictionProtection: true       # 压力时不驱逐该租户
  throttleBeforeEvict: true      # 先限流再驱逐

status:
  currentQuota:
    cpu: "10"
    memory: "40Gi"
  actualUsage:
    cpu: "6.2"
    memory: "28Gi"
  utilizationRate: 0.72
  lastReconcileTime: "2025-01-01T10:00:00Z"
```

### OvercommitPolicy — 全局超卖策略

```yaml
apiVersion: overcommit.io/v1alpha1
kind: OvercommitPolicy
metadata:
  name: cluster-overcommit-policy
spec:
  # 集群整体超卖上限
  globalOvercommitRatio:
    cpu: 1.8
    memory: 1.6
  
  # 触发回收的阈值
  pressureThresholds:
    nodeMemoryPressure: 85%      # 节点内存 >85% 触发回收
    clusterCpuUtilization: 80%   # 集群 CPU >80% 触发限流
  
  # 动态调整策略
  scalingPolicy:
    reconcileInterval: 30s
    cooldownPeriod: 120s         # 调整后冷静期，避免抖动
    stepSize: 10%                # 每次最多调整 10%
    
  # 按层级定义回收顺序
  evictionOrder:
    - tier: silver
      action: throttle_then_evict
    - tier: gold
      action: throttle_only
    - tier: platinum
      action: never
```



## Operator 核心控制逻辑

控制器分四个协作组件，下面是各自的关键算法：

### 1. Metrics Collector — 数据采集

```go
// 每 30s 从 metrics-server 聚合各 Namespace 实际用量
func (c *MetricsCollector) CollectTenantMetrics(ctx context.Context) (map[string]ResourceUsage, error) {
    podMetrics, err := c.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
    
    tenantUsage := make(map[string]ResourceUsage)
    for _, pm := range podMetrics.Items {
        tenant := getTenantFromNamespace(pm.Namespace)
        for _, c := range pm.Containers {
            tenantUsage[tenant].CPU.Add(c.Usage[corev1.ResourceCPU])
            tenantUsage[tenant].Memory.Add(c.Usage[corev1.ResourceMemory])
        }
    }
    return tenantUsage, nil
}
```
### 2. Quota Reconciler — 动态分配引擎

```go
// 核心算法：基于实际使用率重新分配可用额度
func (r *QuotaReconciler) CalcDynamicQuota(
    profiles []TenantProfile,
    clusterCapacity ResourceCapacity,
    usage map[string]ResourceUsage,
    policy OvercommitPolicy,
) map[string]ResourceQuota {
    
    // Step 1: 计算集群可超卖总池
    totalPool := ResourcePool{
        CPU:    clusterCapacity.CPU * policy.GlobalOvercommitRatio.CPU,
        Memory: clusterCapacity.Memory * policy.GlobalOvercommitRatio.Memory,
    }
    
    // Step 2: 先锁定高优先级租户的基准配额
    reserved := reserveBaseQuota(profiles, tier="platinum")
    available := totalPool - reserved
    
    // Step 3: 按利用率给 gold/silver 动态分配剩余池
    // 空闲率越高，下次获得的弹性额度越少（鼓励合理申请）
    for _, p := range nonPlatinumProfiles {
        utilRate := usage[p.Name] / p.CurrentQuota
        elasticQuota := calcElasticShare(utilRate, available, p.OvercommitRatio)
        newQuota := clamp(p.BaseQuota + elasticQuota, p.BaseQuota, p.BurstLimit)
        result[p.Name] = newQuota
    }
    return result
}

// 弹性份额计算：利用率高 → 给更多，利用率低 → 收回
func calcElasticShare(utilRate float64, available Resource, ratio float64) Resource {
    // 平滑函数，避免极端值
    weight := math.Tanh(utilRate * 2)   // [0,1] 区间平滑映射
    return available * weight * ratio
}
```

### 3. Pressure Detector — 压力检测与回收

```go
func (d *PressureDetector) CheckAndReclaim(ctx context.Context) {
    nodes, _ := d.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
    
    for _, node := range nodes.Items {
        for _, cond := range node.Status.Conditions {
            if cond.Type == corev1.NodeMemoryPressure && cond.Status == corev1.ConditionTrue {
                // 触发梯度回收：先从最低优先级开始
                d.reclaimByPriority(ctx, node.Name, []Tier{Silver, Gold})
            }
        }
    }
}

func (d *PressureDetector) reclaimByPriority(ctx context.Context, nodeName string, tiers []Tier) {
    for _, tier := range tiers {
        reclaimed := d.throttleTenantOnNode(ctx, nodeName, tier)
        if reclaimed > PressureReliefTarget {
            break  // 压力已缓解，停止回收
        }
    }
}
```


---

## 关键设计决策

**超卖系数分层** — Platinum 租户 CPU 不超卖（ratio=1.0），保障核心业务；Silver 租户 memory 可以 2x 超卖，利用夜间闲置资源跑批处理任务。

**冷静期（cooldown）机制** — 每次 Reconcile 后强制等待 120s 才能再调整，防止调度抖动导致"配额震荡"，特别是弹性工作负载频繁扩缩容的场景。

**渐进式回收** — 节点压力触发时，先对 Silver 限流（降低 CPU limit），观察 60s；压力未缓解再驱逐 Silver Pod；再不够才对 Gold 限流，但绝不驱逐 Platinum。

**Webhook 准入控制** — 配合 MutatingAdmissionWebhook，在 Pod 创建时动态注入 `requests/limits` 比例，让低优先级工作负载自动使用 BestEffort QoS，便于系统在内存压力下优先回收。

**可观测性** — Operator 暴露 Prometheus 指标：`tenant_quota_utilization_ratio`、`overcommit_reclaim_events_total`、`tenant_eviction_count`，供 Grafana 大盘展示集群超卖健康度。

## 正常情况（集群不繁忙）

① 管理员提交 TenantProfile YAML → 注册租户
② 管理员提交 OvercommitPolicy YAML → 设定规则
③ Operator 启动，开始每 30s 的工作循环
④ Operator 发现集群还有 40% 空闲
⑤ 计算出 Silver 租户 C 可以从 20 CPU 扩到 38 CPU
⑥ 更新租户 C 的 ResourceQuota → 38 CPU
⑦ 租户 C 的 Pod 可以申请更多资源，正常运行

压力情况（集群变忙）

① 租户 A（Platinum）业务高峰，CPU 需求飙升
② Operator 检测到集群 CPU 利用率超过 80%
③ 触发回收流程：
   → 先把 Silver 租户 C 的动态配额缩回基准配额（20 CPU）
   → 如果还不够，限制 Gold 租户 B 的 CPU 带宽
   → Platinum 租户 A 完全不受影响
④ 租户 C 的新 Pod 无法申请超额资源（被 ResourceQuota 拦住）
⑤ 租户 C 已运行的 Pod 如果超限，可能被驱逐
⑥ 高峰过去后，Operator 再逐步把配额还回去

设计上的防抖机制

问题：如果配额每 30 秒大幅波动，Pod 会频繁重启，反而更乱。

解决：
  • 每次最多调整 10%（stepSize），不允许突变
  • 每次调整后等 120 秒才能再调（cooldown）
  • 用平滑函数计算弹性份额，避免极端值

效果：配额变化是缓慢渐进的，业务无感知。