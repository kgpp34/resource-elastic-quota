# 多集群全局配额控制器分层演进计划

Copyright kgpp34 2026.

## 1. 文档目的

本文档定义 `resource-elastic-quota` 从当前单集群实现演进为多集群全局配额系统的目标架构、API 边界、分阶段交付物和验收标准。

已确认的方向是直接采用全局配额控制器，不经历“各集群分别由管理员维护独立部门 Quota”的产品阶段。当前已经完成的单集群核算、动态分配和 Webhook 不会废弃，而是演进为成员集群执行单元。

第一版多集群能力继续遵守现有约束：

- 只核算和限制 Pod aggregate `limits.memory`。
- requests 和 CPU 不进入部门额度统计或拒绝判断。
- 删除、缩容和降低 memory limit 始终允许。
- 普通 Quota 回收不自动驱逐 Pod。
- 支持 Kubernetes 1.23 及以上版本。
- 全局控制面负责额度分配，成员集群本地 Webhook 负责最终准入。

## 2. 目标架构

```text
                        Global Control Plane
┌────────────────────────────────────────────────────────────┐
│ Global Quota Controller                                    │
│                                                            │
│ MemberCluster                                               │
│ PoolClass                                                   │
│ GlobalDepartmentQuota                                       │
│ ClusterResourceReport                                       │
│ ClusterQuotaLease                                           │
│                                                            │
│ 全局容量汇总 -> 全局公平分配 -> 生成集群额度租约              │
└────────────────────────────┬───────────────────────────────┘
                             │
                    成员集群主动连接和同步
              ┌──────────────┴──────────────┐
              ▼                             ▼
┌─────────────────────────┐   ┌─────────────────────────┐
│ Member Cluster A        │   │ Member Cluster B        │
│                         │   │                         │
│ Member Agent            │   │ Member Agent            │
│ Local Accounting        │   │ Local Accounting        │
│ Local ResourcePool      │   │ Local ResourcePool      │
│ Local Lease Snapshot    │   │ Local Lease Snapshot    │
│ Local Webhook           │   │ Local Webhook           │
└─────────────────────────┘   └─────────────────────────┘
```

全局控制面建议部署在专门的管理集群中。成员集群主动访问管理集群，管理集群不保存全部成员集群的 cluster-admin kubeconfig，也不在 Admission 请求路径上远程访问成员集群。

本地 Webhook 不同步调用全局控制器。全局控制器下发互不重叠的额度租约，本地 Webhook 只读取本地 informer cache 和不可变租约快照。这样管理集群或跨集群网络短暂故障不会阻塞所有成员集群的 kube-apiserver。

## 3. 全局核算模型

### 3.1 核算维度

当前单集群账本：

```text
department × ResourcePool
```

多集群账本：

```text
department × cluster × PoolClass
```

其中：

- `department` 是可信 Namespace 绑定的业务部门。
- `cluster` 是稳定且唯一的成员集群身份。
- `PoolClass` 是跨集群统一的逻辑节点类型，例如 `amd64-kylin`。
- 成员集群本地 `ResourcePool` 映射到一个全局 `PoolClass`。

### 3.2 核心不变量

全局部门额度：

```text
baseQuota <= globalEffectiveQuota <= maxQuota
```

全局租约总量：

```text
sum(active ClusterQuotaLease for department and PoolClass)
    <= globalEffectiveQuota
```

成员集群部门准入：

```text
localDepartmentAllocated + admissionDelta
    <= ClusterQuotaLease.quota
```

成员集群资源池准入：

```text
localPoolAllocated + admissionDelta
    <= local ResourcePool admissionBudget
```

全局额度不能替代本地物理容量检查。即使一个部门仍有全局余额，目标成员集群对应 ResourcePool 已达到 admissionBudget 时，本地 Webhook 仍必须拒绝正向增量。

### 3.3 一致性原则

- 全局控制器只分配租约，不参与每一次 Pod Admission。
- 不在 Admission 请求中写中央状态做资源预占。
- 不允许两个成员集群同时持有同一份全局额度。
- 未确认释放的失联集群额度不能立即重新分配。
- 租约过期不等于已有 Pod 消失，不能因此假定 allocated 已释放。
- 现有 Pod 不因租约过期或额度回收被自动驱逐。

## 4. 全局 API 规划

全局 API 建议使用独立 API Group：

```text
multicluster.quota.kgpp34.io/v1alpha1
```

成员集群本地 API 保持：

```text
quota.kgpp34.io/v1alpha1
```

### 4.1 MemberCluster

`MemberCluster` 表示一个注册到全局控制面的成员集群。

```yaml
apiVersion: multicluster.quota.kgpp34.io/v1alpha1
kind: MemberCluster
metadata:
  name: production-shanghai-01
spec:
  enabled: true
  region: shanghai
  labels:
    environment: production
status:
  agentVersion: v0.1.0
  kubernetesVersion: v1.28.0
  lastHeartbeatTime: "2026-07-17T10:00:00Z"
  connectionState: Ready
  reportEpoch: 15
```

`metadata.name` 作为 ClusterID，必须稳定且唯一，不能使用 Pod UID、Service ClusterIP 或可能变化的 apiserver 地址。

### 4.2 PoolClass

`PoolClass` 定义跨集群统一的逻辑节点类型。

```yaml
apiVersion: multicluster.quota.kgpp34.io/v1alpha1
kind: PoolClass
metadata:
  name: amd64-kylin
spec:
  resources:
    - memory
```

成员集群本地 ResourcePool 增加映射：

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: ResourcePool
metadata:
  name: production-amd64
spec:
  poolClass: amd64-kylin
  nodeSelector:
    matchLabels:
      nodetype.cks.io/arch: amd64
      nodetype.cks.io/os: kylin
```

全局控制器只理解 PoolClass 和报告的容量，不解析各成员集群的 NodeSelector。具体节点分类仍由成员 Agent 完成。

### 4.3 GlobalDepartmentQuota

`GlobalDepartmentQuota` 是部门额度的唯一配置来源。

```yaml
apiVersion: multicluster.quota.kgpp34.io/v1alpha1
kind: GlobalDepartmentQuota
metadata:
  name: department-a
spec:
  department: department-a
  pools:
    - poolClass: amd64-kylin
      baseQuota:
        memory: 200Gi
      maxQuota:
        memory: 500Gi
      weight: 100
      targetUtilizationPercent: 80
      growthHeadroomPercent: 20
      reclaimAfter: 10m
      placement:
        allowedClusters:
          matchLabels:
            environment: production
status:
  pools:
    - poolClass: amd64-kylin
      allocatedLimits:
        memory: 240Gi
      effectiveQuota:
        memory: 360Gi
      leasedQuota:
        memory: 340Gi
```

普通成员集群和部门用户不能修改 GlobalDepartmentQuota。

### 4.4 ClusterResourceReport

成员 Agent 使用 `ClusterResourceReport` 上报本地容量、压力、分配量和需求。

```yaml
apiVersion: multicluster.quota.kgpp34.io/v1alpha1
kind: ClusterResourceReport
metadata:
  name: production-shanghai-01
spec:
  clusterID: production-shanghai-01
  epoch: 15
  sequence: 1024
  observedAt: "2026-07-17T10:00:00Z"
  pools:
    - localPool: production-amd64
      poolClass: amd64-kylin
      allocatable:
        memory: 640Gi
      admissionBudget:
        memory: 936Gi
      allocatedLimits:
        memory: 520Gi
      memoryPressure: false
  departments:
    - department: department-a
      poolClass: amd64-kylin
      allocatedLimits:
        memory: 80Gi
      observedUsage:
        memory: 41Gi
      pendingDemand:
        memory: 20Gi
```

中央控制器必须拒绝低于已接受 epoch 或 sequence 的旧报告。

### 4.5 ClusterQuotaLease

`ClusterQuotaLease` 是全局控制器授予某集群、部门和 PoolClass 的额度切片。

```yaml
apiVersion: multicluster.quota.kgpp34.io/v1alpha1
kind: ClusterQuotaLease
metadata:
  name: production-shanghai-01-department-a-amd64-kylin
spec:
  clusterID: production-shanghai-01
  department: department-a
  poolClass: amd64-kylin
  quota:
    memory: 120Gi
  epoch: 18
  validUntil: "2026-07-17T10:30:00Z"
status:
  acknowledgedEpoch: 18
  localAllocated:
    memory: 80Gi
  lastAcknowledgedTime: "2026-07-17T10:00:05Z"
```

成员 Agent 只能读取属于自身 ClusterID 的租约并更新确认状态，不能修改租约 Spec。

## 5. 分层演进阶段

### 阶段 0：冻结全局语义和安全边界

#### 目标

在编码前固定额度含义、失联处理、ClusterID 和控制面部署位置，避免后续 CRD 反复修改。

#### 交付物

- 核算维度和核心不变量。
- 管理集群部署拓扑。
- ClusterID 命名与生命周期规则。
- PoolClass 命名和本地 ResourcePool 映射规则。
- 网络分区时的安全优先策略。
- 全局 CRD 字段草案和状态机。

#### 必须确认的策略

- 删除、缩容和降低 memory limit 永远放行。
- 中央不可用时已有 Pod 不受影响。
- 有效租约内是否允许继续增长；建议允许。
- 租约过期后的正向增量；建议拒绝。
- 未确认释放的额度；建议继续保留，不重新分配。
- 自动驱逐；继续默认关闭。

#### 验收标准

- 不存在影响 API 结构的未决语义。
- 全局和本地额度边界没有重叠职责。
- 确认安全优先的故障行为。

### 阶段 1：将当前 Controller 演进为 Member Agent

#### 目标

保留现有单集群功能，同时解除 Webhook 对本地 DepartmentQuota 配置来源的直接依赖。

#### 代码结构

```text
cmd/
├── global-manager/
│   └── main.go
└── member-agent/
    └── main.go

internal/
├── accounting/
├── allocation/
├── webhook/
├── member/
└── global/
```

#### 关键改造

为本地准入引入小接口：

```go
type QuotaGrantReader interface {
    GetGrant(
        ctx context.Context,
        department string,
        pool string,
    ) (QuotaGrant, error)
}
```

允许存在两个实现：

- `StandaloneGrantReader`：本地兼容和测试用途。
- `LeaseGrantReader`：正式多集群模式。

生产多集群部署只启用 LeaseGrantReader。Standalone 模式不得成为另一套长期配置来源。

#### 交付物

- `cmd/member-agent` 入口。
- 稳定 ClusterID 配置和状态字段。
- QuotaGrantReader 抽象。
- 本地不可变租约快照结构。
- 现有核算、分配和 Webhook 回归测试。

#### 验收标准

- 当前单集群测试全部通过。
- 本地 Webhook 已通过接口读取额度。
- 中央控制面离线不会影响已有 Pod、删除和缩容。
- 成员状态可以携带 ClusterID。

### 阶段 2：实现全局 API 和成员集群注册

#### 目标

建立 Global Manager、全局 CRD 和可信成员身份。

#### 连接模型

- Member Agent 主动访问管理集群。
- 每个成员集群使用独立凭证。
- 认证身份与 ClusterID 绑定。
- 管理集群不主动持有成员集群管理员 kubeconfig。

#### 最小 RBAC

成员身份只能：

- 更新自己的 MemberCluster status。
- 创建或更新自己的 ClusterResourceReport。
- 读取属于自己的 ClusterQuotaLease。
- 更新自己的租约确认状态。

成员身份不能：

- 修改 GlobalDepartmentQuota。
- 修改 PoolClass。
- 修改任何 ClusterQuotaLease Spec。
- 冒充其他 ClusterID 上报。

#### 交付物

- MemberCluster、PoolClass、GlobalDepartmentQuota、ClusterResourceReport 和 ClusterQuotaLease CRD。
- `cmd/global-manager` 入口。
- 注册、心跳和身份校验 Controller。
- 每集群最小 RBAC 和凭证轮换方案。

#### 验收标准

- 成员集群可以注册并周期心跳。
- 重复 ClusterID 或身份不匹配被拒绝。
- 本地 ResourcePool 到 PoolClass 的映射可校验。
- 管理集群无需访问成员集群 apiserver。

### 阶段 3：实现本地资源报告链路

#### 目标

让全局控制面获得所有成员集群的容量、分配量、压力和需求快照，但暂不影响准入。

#### 上报规则

- Agent 注册代数使用单调递增 epoch。
- 同一 epoch 的报告使用单调递增 sequence。
- 报告来源是本地 informer cache 的一致快照。
- limits 数据不依赖 metrics-server。
- 实际用量不可用时只标记 MetricsDegraded。
- 使用差异 Patch 和固定上报周期抑制写入。
- 报告超过 maxReportAge 后集群进入 Stale。

#### 交付物

- Member Agent reporter 和 heartbeat。
- Global Manager report validator。
- 集群和部门全局只读汇总状态。
- 过期报告 Condition、Event 和指标。

#### 验收标准

- Pod、Node 和 ResourcePool 变化能收敛到中央报告。
- 旧 epoch/sequence 不能覆盖新报告。
- Agent 重启后能重建完整状态。
- metrics-server 故障不影响 limits 报告。
- 本阶段不会改变任何集群的准入行为。

### 阶段 4：完成静态全局额度租约闭环

#### 目标

先实现“全局配置 -> 静态租约 -> 成员同步 -> 本地准入”的完整闭环，不同时引入动态跨集群借用。

#### 流程

```text
GlobalDepartmentQuota
        |
        v
Global Manager 生成静态 ClusterQuotaLease
        |
        v
Member Agent 拉取并校验租约
        |
        v
原子替换本地 QuotaGrant Snapshot
        |
        v
Local Webhook 执行最终准入
```

#### 本地准入规则

```text
departmentAllocated + delta <= lease.quota
```

并且：

```text
poolAllocated + delta <= ResourcePool.admissionBudget
```

#### 交付物

- 静态租约分配器。
- Member Agent lease syncer。
- 租约 ClusterID、epoch、validUntil 校验。
- 本地 LeaseGrantReader。
- 租约确认状态回写。
- Observe、Warn、Enforce 联调。

#### 验收标准

- GlobalDepartmentQuota 是唯一人工额度配置来源。
- 中央额度变更能收敛到本地准入结果。
- 本地用户不能修改生效额度。
- 无有效租约时正向增量按策略拒绝。
- 租约缺失或过期时删除、缩容仍放行。
- 所有有效租约总和不超过全局部门额度。

阶段 4 是第一个可投入 Observe/Warn 试运行的多集群版本。

### 阶段 5：实现全局动态分配

#### 目标

根据成员报告，在部门之间和成员集群之间动态分配额度租约。

#### 两层分配

第一层按 PoolClass 在部门之间分配：

```text
PoolClass 全局可用名义额度
    -> Department globalEffectiveQuota
```

第二层把部门额度分配到成员集群：

```text
globalEffectiveQuota
    -> ClusterQuotaLease[cluster-a]
    -> ClusterQuotaLease[cluster-b]
```

#### 集群需求

对于集群 c、部门 d、PoolClass p：

```text
utilizationTarget = allocated[c,d,p] / targetUtilization
growthTarget = allocated[c,d,p] * (1 + growthHeadroomPercent)
desired[c,d,p] = max(allocated, utilizationTarget, growthTarget)
```

desired 还受以下条件限制：

- GlobalDepartmentQuota maxQuota。
- allowedClusters 和集群权重。
- 集群本地 admissionBudget。
- MemoryPressure 和报告时效。
- 当前未确认释放额度。
- maxIncreasePercent、maxDecreasePercent、cooldown 和 reclaimAfter。

#### 分配顺序

1. 保留所有成员报告的 allocated。
2. 保留未确认释放的 heldQuota。
3. 分配显式配置的集群最小切片；若启用该可选字段。
4. 计算各集群额外需求。
5. 按部门权重执行全局 weighted max-min。
6. 在部门内部按集群需求和权重分配。
7. 达到本地池容量的集群退出后续分配。
8. 为变化结果生成新的租约 epoch。

#### 验收标准

- 所有租约总和不超过 globalEffectiveQuota。
- globalEffectiveQuota 不超过 maxQuota。
- 集群压力或报告过期时不增加租约。
- 需求增长的集群可以获得额度。
- 低需求持续超过 reclaimAfter 后归还额度。
- 相同输入产生确定性结果。
- 所有 memory 运算保持 Quantity 精度且有溢出保护。

### 阶段 6：实现租约状态机、网络分区和 Fencing

#### 目标

确保控制面重启、Agent 重启、网络分区和集群恢复时不会重复发放额度。

#### 租约状态机

```text
Pending -> Active -> Renewing -> Stale -> Expired -> Replaced
```

#### Epoch Fencing

- 每次重新分配生成更高 epoch。
- Member Agent 只能接受更高 epoch，不能回退。
- 中央收到新 epoch 确认后才能把旧租约标记为 Replaced。
- 旧 Agent 或旧凭证不能覆盖新租约状态。

#### 网络分区保护

集群失联后保留：

```text
heldQuota = max(
    lastReportedAllocated,
    lastAcknowledgedProtectedQuota
)
```

可以重新分配的最多是：

```text
reclaimable = oldLeaseQuota - heldQuota
```

无法确认原集群状态时，heldQuota 继续占用全局额度。

#### 本地租约过期

- 已有 Pod 不驱逐。
- 删除、缩容和降低 limit 放行。
- 新的正向增量拒绝。
- 设置 LeaseExpired Condition 并告警。
- 中央恢复后续租或下发更高 epoch。

#### 永久故障集群

只有平台管理员可以执行显式 force release。该操作必须：

- 使用单独 RBAC。
- 要求集群失联超过安全窗口。
- 记录操作者、原因和时间。
- 永久废弃旧 ClusterID epoch。
- 要求恢复的旧集群重新注册。

#### 验收标准

- 网络分区不会导致同一份额度在两个集群有效。
- 旧 Agent 不能使用旧 epoch 覆盖新状态。
- 租约过期不驱逐已有 Pod。
- Global Manager 重启后能从 CRD 重建租约状态。
- force release 有权限、时间和审计保护。

达到阶段 6 后，系统才具备生产多集群额度再分配所需的完整故障语义。

### 阶段 7：接入多集群部署平台

#### 目标

向部署平台提供候选集群信息，减少工作负载提交到额度或物理容量不足集群的概率。

#### 查询内容

```text
clusterID
PoolClass
department lease remaining
local admission remaining
pressure state
report age
lease expiry
```

#### 选址流程

```text
部署平台查询候选集群
        |
        v
排除报告过期、租约不足或 MemoryPressure 集群
        |
        v
选择目标集群并提交工作负载
        |
        v
目标集群 Local Webhook 最终准入
```

查询结果不是资源预留，本地 Webhook 始终保留最终拒绝权。第一版不同时引入 PlacementReservation；只有实际并发选址证明需要后再单独设计。

#### 验收标准

- 平台不会主动选择已知额度不足的集群。
- 查询接口只读，不影响 Admission 可用性。
- 查询结果过期时平台能重试或重新选址。
- 平台和本地 Webhook 使用相同的部门与 PoolClass 语义。

### 阶段 8：生产化、可观测性和上线

#### 全局指标

```text
global_quota_cluster_connected
global_quota_cluster_report_age_seconds
global_quota_department_effective
global_quota_cluster_lease
global_quota_cluster_lease_remaining
global_quota_held_quota
global_quota_lease_expired_total
global_quota_allocation_duration_seconds
global_quota_fencing_rejections_total
global_quota_force_release_total
```

指标不得使用 Pod 名称或 UID 等高基数字段。

#### 高可用

Global Manager：

- 至少两个副本并启用 Leader Election。
- CRD 是事实来源，内存只保存可重建快照。
- 重启后执行全量重算。
- 配置 PDB 和跨节点反亲和。
- 管理集群 CRD 和凭证必须纳入备份。

Member Agent 和 Webhook：

- Webhook 至少两个副本。
- Member Controller 使用 Leader Election。
- Webhook 无状态读取本地不可变快照。
- 中央连接异常不影响已有对象、删除和缩容。

#### 上线顺序

1. 注册测试成员集群，只上报不分配。
2. 生成静态租约，本地 Webhook 使用 Observe。
3. 对比中央租约和成员实际 allocated。
4. 切换 Warn。
5. 单部门、单集群进入 Enforce。
6. 扩大到同一部门的多个集群。
7. 开启动态跨集群租约调整。
8. 完成网络分区和 Fencing 演练。
9. 接入部署平台选址。

## 6. 建议代码结构

```text
cmd/
├── global-manager/
│   └── main.go
└── member-agent/
    └── main.go

api/
├── quota/
│   └── v1alpha1/
│       ├── resourcepool_types.go
│       └── localquotalease_types.go
└── multicluster/
    └── v1alpha1/
        ├── membercluster_types.go
        ├── poolclass_types.go
        ├── globaldepartmentquota_types.go
        ├── clusterresourcereport_types.go
        └── clusterquotalease_types.go

internal/
├── accounting/
├── allocation/
├── webhook/
├── member/
│   ├── reporter.go
│   ├── lease_syncer.go
│   └── heartbeat.go
└── global/
    ├── registration_controller.go
    ├── report_controller.go
    ├── allocator.go
    ├── lease_controller.go
    └── fencing.go
```

当前 `api/v1alpha1` 不需要在第一步立即物理移动。建议先引入新入口和接口，在阶段 2 创建全局 API 时再有计划地调整包路径，避免一次提交同时包含大规模目录迁移和行为变化。

## 7. 测试矩阵

### 单元测试

- 全局额度和租约不变量。
- 两层 weighted max-min 分配。
- PoolClass 和本地 ResourcePool 映射。
- epoch、sequence 和租约状态转换。
- heldQuota 和 reclaimable 计算。
- 报告过期、MemoryPressure 和 cooldown。
- Quantity 精度和溢出。

### Envtest 集成测试

- 全局 CRD schema 和 validation。
- 成员注册和身份边界。
- 报告乱序和旧 epoch 拒绝。
- GlobalDepartmentQuota 到 ClusterQuotaLease。
- 租约确认和替换。
- Global Manager 重启重建。

### 多集群 E2E

- 一个管理集群和至少两个成员 Kind 集群。
- 同一部门跨集群 limits.memory 汇总。
- 静态租约、本地准入和全局总量约束。
- 动态额度从低需求集群转移到高需求集群。
- 中央停止、成员失联和网络恢复。
- 旧 Agent 和旧 epoch Fencing。
- 租约过期时增长拒绝、删除缩容放行。
- force release 审计和旧集群重新注册。
- 全局控制器和本地 Webhook 滚动升级。

## 8. 里程碑建议

| 里程碑 | 包含阶段 | 可用能力 |
| --- | --- | --- |
| M1 | 0～3 | 全局注册和只读资源汇总，不影响准入 |
| M2 | 4 | 静态全局额度租约闭环，可进入 Observe/Warn |
| M3 | 5 | 动态跨集群额度分配 |
| M4 | 6 | 网络分区、过期租约和 Fencing 完整语义 |
| M5 | 7～8 | 部署平台选址、生产可观测性和正式上线 |

不建议跳过 M2 直接实现动态分配，也不建议在完成阶段 6 前把失联集群额度自动全部转移到其他集群。

## 9. 第一轮编码范围建议

下一轮编码建议只启动阶段 1：

1. 将当前 manager 明确为 Member Agent。
2. 增加 ClusterID 配置和状态传递。
3. 抽取 QuotaGrantReader。
4. 增加本地不可变 QuotaGrant Snapshot。
5. 用 StandaloneGrantReader 保持现有行为和测试。
6. 为后续 LeaseGrantReader 保留接口，但不提前创建全局 CRD。

阶段 1 完成并回归通过后，再单独启动全局 CRD 和 Global Manager，能够把结构性重构与分布式状态语义分开验证。
