# 弹性资源 Quota Controller 设计与实施方案

## 1. 文档目的

本文档定义一个基于 Kubebuilder 实现的 Kubernetes 弹性资源 Quota Controller，作为后续编码、测试和验收的依据。

设计目标包括：

- 支持 Kubernetes 1.23 及以上版本。
- 支持 ARM64/Kylin、AMD64/Kylin 等异构节点，并允许后续增加更多节点类型。
- 第一版按部门和节点资源池统计 Pod 的 `limits.memory` 和内存实际观测用量。
- 为每个部门配置历史基础 Quota 和可借用最高上限，并在部门之间动态借用空闲额度。
- 在部门资源超限时，通过 admission webhook 阻止创建或扩大计算资源占用。
- 明确强一致性、故障处理和安全边界，避免 controller 对生产集群造成不可控影响。

第一版资源范围明确为 memory-only：

- Pod requests（包括 `requests.memory`）不计入 DepartmentQuota。
- CPU requests/limits 不计入 DepartmentQuota。
- Webhook 不因 CPU 用量或 CPU 配额拒绝对象。
- Controller 不根据 CPU 使用率发放或回收额度。
- 内部资源运算仍基于 `corev1.ResourceList`，避免以后增加 CPU 时修改 CRD 结构和核心算法接口。

本文档不会直接沿用《研发环境资源动态超卖controller.md》的实现方案。原文档可作为需求背景，但其中的统计口径、配额算法和准入假设需要修正。

## 2. 需求解释与边界

### 2.1 部门身份

业务 Pod 均带有：

```yaml
metadata:
  labels:
    department: department-a
```

但不能仅信任用户提交的 `department` 标签，否则用户可以把标签修改成其他部门以绕过配额。

受管理的 Namespace 必须由平台管理员增加不可由普通用户修改的标签：

```yaml
metadata:
  labels:
    quota.kgpp34.io/enforced: "true"
    quota.kgpp34.io/department: department-a
```

Webhook 必须校验 Pod 或工作负载模板中的 `department` 与 Namespace 上的 `quota.kgpp34.io/department` 完全一致。

本项目采用“显式纳管”而不是维护系统 Namespace 黑名单：

- 只有同时带有 `quota.kgpp34.io/enforced=true` 和 `quota.kgpp34.io/department=<部门>` 的业务 Namespace 才参与统计和准入。
- `kube-system`、`koordinator-system` 等没有部门归属且未带 `enforced=true` 的 Namespace 不进入 Controller 核算，Webhook 也不拦截其中的工作负载。
- Namespace 只有 department、没有 `enforced=true` 时不启用配额，便于分阶段上线。
- Namespace 有 `enforced=true`、但缺少 department 时视为平台配置错误；Controller 告警，Webhook 在 Enforce 模式下拒绝其中新增的业务工作负载。
- 不在代码中硬编码 `kube-system`、`koordinator-system` 名称。使用纳管标签可以自动兼容以后增加的系统组件 Namespace。

这里的“不纳管”是指系统 Namespace 不建立 DepartmentQuota 账本、Webhook 不限制其工作负载。已经调度到某个 ResourcePool 的系统 Pod 仍计入该池的 aggregate `limits.memory`，因为它们真实消耗同一批节点容量；否则业务部门可用预算会被高估。

### 2.2 “资源使用量”的定义

系统同时维护三类资源数据：

| 数据 | 来源 | 用途 | 是否作为准入依据 |
| --- | --- | --- | --- |
| Allocated Memory Limits | Pod Spec | 内存配额统计和准入 | 是 |
| Observed Memory Usage | metrics-server 或监控系统 | 内存超卖风险、利用率和告警 | 否 |
| Effective Quota | Controller 动态计算 | 当前允许部门使用的额度 | 是 |

本 Controller 的部门配额口径固定为容器声明的 `limits.memory`。Kubernetes 调度器仍根据 Pod requests 独立完成节点放置，但 `requests.memory` 不进入部门配额统计、动态分配或 Webhook 超限判断。实际用量也不能替代 limit 成为严格准入依据。

Observed Usage 可以用于：

- 判断借用额度是否长期闲置。
- 评估内存超卖风险。
- 输出利用率指标和告警。
- 为后续容量规划提供数据。

metrics-server 不可用时，Controller 仍须正常统计 `limits.memory`，Webhook 仍须正常执行 limit 配额准入。

### 2.3 “禁止提交任何 Kubernetes 资源”的解释

超限后不应禁止 ConfigMap、Secret、Event、删除或缩容等所有 API 操作。这些操作不会增加计算资源，而且完全禁止会阻碍用户修复超限状态。

本设计将该需求解释为：

> 当部门内存超限时，禁止创建或扩大任何会增加该部门 `limits.memory` 的 Pod 或工作负载；始终允许删除、缩容和降低内存 limit。单独增加 request 不受本 Controller 限制。

## 3. 原参考方案的主要问题

《研发环境资源动态超卖controller.md》存在以下问题，不能直接转化为实现：

1. metrics-server 的瞬时用量不等于 Kubernetes 已分配资源，不能作为严格准入依据。
2. ResourceQuota 仅按 Namespace 聚合，不能按 `department` 标签或节点类型隔离。
3. `available * tanh(utilization) * ratio` 对每个部门重复使用整个 available，可能导致最终分配总和超过资源池容量。
4. 动态降低 ResourceQuota 不会影响已经创建的 Pod，只会阻止后续对象。
5. Kubernetes 1.23 不支持直接修改运行中 Pod 的 CPU/内存 requests 和 limits；所谓“先修改 limit 限流”需要重建 Pod，不能作为通用回收手段。
6. 将低优先级 Pod 自动修改为 BestEffort 会失去 limits 统计基础，并增加节点 OOM 和驱逐风险。
7. `evictionProtection: true` 不是 Kubernetes 提供的强保证。PodPriority、PDB 和节点压力驱逐分别有各自语义。
8. 原方案没有解决并发 admission 请求读取旧配额状态造成的穿透问题。

## 4. 总体架构

```text
Node / Pod / DepartmentQuota / ResourcePool 事件
                     │
                     ▼
          ElasticQuotaPolicy Controller
          ├── 节点资源池容量计算
          ├── 部门 limits.memory 聚合
          ├── 动态借用与回收算法
          ├── 可选实际用量采集
          └── 更新 CRD Status 和准入快照
                     │
                     ▼
              Validating Webhook
          ├── 校验部门身份
          ├── 校验目标资源池
          ├── 计算本次资源增量
          └── 允许或拒绝 CREATE/UPDATE
```

第一版包含三个 CRD：

- `ResourcePool`：描述一种节点资源池及其容量策略。
- `DepartmentQuota`：描述一个部门在各资源池中的历史基础 Quota 和可借用最高上限。
- `ElasticQuotaPolicy`：描述集群级动态分配、回收和准入策略。

Controller 采用事件驱动 reconcile，并通过定期全量校正确保最终一致性。

## 5. API 设计

建议使用 API Group：

```text
quota.kgpp34.io/v1alpha1
```

所有比例字段建议使用字符串形式的 `resource.Quantity`，例如 `"1.5"`，避免使用浮点数产生精度和序列化问题。

### 5.1 ResourcePool

`ResourcePool` 为 Cluster Scoped，一个对象代表一种可独立核算的节点资源池。

ARM64/Kylin 示例：

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
    # 从 Node status.allocatable 汇总值中额外扣除的平台保留量。
    reserve:
      memory: "16Gi"

    # 所有已准入 Pod 的 aggregate limits.memory 上限。
    admissionRatio:
      memory: "1.5"

    # 部门名义额度总和允许超过物理容量的比例。
    entitlementRatio:
      memory: "2.0"

status:
  observedGeneration: 1
  readyNodes: 10
  allocatable:
    memory: "640Gi"
  admissionBudget:
    memory: "936Gi"
  configuredEntitlementBudget:
    memory: "1248Gi"
  effectiveEntitlementBudget:
    memory: "1248Gi"
  allocatedLimits:
    memory: "520Gi"
  conditions: []
```

AMD64/Kylin 示例：

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: ResourcePool
metadata:
  name: amd64-kylin
spec:
  nodeSelector:
    matchLabels:
      nodetype.cks.io/arch: amd64
      nodetype.cks.io/os: kylin
```

后续新增节点类型时，只需新增 ResourcePool，不应修改 Controller 中的枚举或 switch。

#### 节点容量规则

- 使用 `Node.status.allocatable`，不使用 `Node.status.capacity`。
- 新借用额度只使用 Ready 且可调度的节点容量。
- `spec.unschedulable=true` 的节点不再提供新增借用容量。
- 已经运行在 cordon 节点上的 Pod 仍计入资源占用。
- NotReady、Unknown 节点上的 Pod 在被删除前仍计入占用，避免故障时错误释放配额。
- 一个 Node 必须唯一匹配一个 ResourcePool。
- 匹配多个 ResourcePool 时设置 `SelectorOverlap=True`，该 Node 不参与新增容量计算。
- 未匹配任何 ResourcePool 时设置指标和事件，但不纳入受管容量。

### 5.2 DepartmentQuota

`DepartmentQuota` 为 Cluster Scoped，一个对象代表一个部门，可以包含多个 ResourcePool 的额度。

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: DepartmentQuota
metadata:
  name: department-a
spec:
  department: department-a

  # 必须与全局 eviction.enabled 同时开启才允许驱逐本部门 Pod。
  evictionPolicy: Deny

  namespaceSelector:
    matchLabels:
      quota.kgpp34.io/enforced: "true"
      quota.kgpp34.io/department: department-a

  pools:
    - name: arm64v8-kylin
      baseQuota:
        memory: "48Gi"
      maxQuota:
        memory: "128Gi"
      weight: 100
      targetUtilizationPercent: 80
      growthHeadroomPercent: 20
      reclaimAfter: 10m

    - name: amd64-kylin
      baseQuota:
        memory: "96Gi"
      maxQuota:
        memory: "320Gi"
      weight: 200
      targetUtilizationPercent: 80
      growthHeadroomPercent: 20
      reclaimAfter: 10m

status:
  observedGeneration: 1
  pools:
    - name: arm64v8-kylin
      allocatedLimits:
        memory: "70Gi"
      observedUsage:
        memory: "39Gi"
      effectiveQuota:
        memory: "100Gi"
      borrowed:
        memory: "40Gi"
      lastDemandTime: "2026-07-17T08:00:00Z"
      lastUpdateTime: "2026-07-17T08:00:00Z"
  conditions:
    - type: Ready
      status: "True"
    - type: OverQuota
      status: "False"
```

字段语义：

- `baseQuota.memory`：历史形成的部门 aggregate `limits.memory` 基础上限。
- `maxQuota.memory`：部门借用空闲资源后 aggregate `limits.memory` 仍不能超过的绝对上限。
- `effectiveQuota`：Controller 当前授予的动态上限，位于 baseQuota 和 maxQuota 之间。
- `evictionPolicy`：`Deny` 或 `Allow`，默认 `Deny`；仅控制严重节点压力下是否允许驱逐，不影响正常 Quota 回收。
- `weight`：共享资源不足时的分配权重。
- `targetUtilizationPercent`：触发提前扩容的目标利用率。
- `growthHeadroomPercent`：避免部门刚达到额度就被阻塞的增长余量。
- `reclaimAfter`：持续低利用率多久后才允许回收借用额度。

`baseQuota` 的准确业务语义是“动态回收不能低于的历史基础 Quota”，不是物理资源保证：

- Controller 不会在正常回收时把部门 effectiveQuota 降到 baseQuota 以下。
- Webhook 仍会同时检查整个 ResourcePool 的 admissionBudget。
- 如果所有部门同时使用各自 baseQuota，资源池可能无法在物理上同时满足；此时后到的正向请求会被资源池总量检查阻止。
- 因此 API 不使用 `guaranteed` 命名，避免给使用方造成物理资源已经预留的误解。

#### 配置校验

Webhook 或 CRD validation 必须拒绝以下配置：

- `baseQuota.memory` 大于 `maxQuota.memory`。
- baseQuota 或 maxQuota 为负数，或包含第一版未启用的资源名。
- 同一个 DepartmentQuota 中出现重复 ResourcePool。
- 引用不存在的 ResourcePool。
- weight 小于等于 0。
- target utilization 不在合理范围内，例如 1～100。
- Namespace 同时匹配多个不同部门。

### 5.3 ElasticQuotaPolicy

`ElasticQuotaPolicy` 为 Cluster Scoped。第一版只允许名为 `default` 的单例。

```yaml
apiVersion: quota.kgpp34.io/v1alpha1
kind: ElasticQuotaPolicy
metadata:
  name: default
spec:
  resources:
    - memory

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
    # 默认关闭。降低 Quota 本身不会驱逐已经运行的 Pod。
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

status:
  observedGeneration: 1
  lastCalculationTime: "2026-07-17T08:00:00Z"
  conditions: []
```

Admission mode：

- `Observe`：只记录本应拒绝的请求，不返回警告、不拒绝。
- `Warn`：允许请求，但通过 AdmissionResponse warnings 和 Event 告警。
- `Enforce`：正式拒绝超限请求。

`spec.resources` 是第一版实际参与统计、分配和准入的资源白名单：

- 第一版固定为 `[memory]`。
- DepartmentQuota 中的 baseQuota/maxQuota 只能配置 `memory`，出现 `cpu` 等未启用资源时拒绝配置，避免管理员误以为 CPU 已受控。
- Pod 的 requests 和 CPU limits 保持原值，但 Controller 不读取它们参与 Quota 计算，Webhook 也不检查这些总量。
- 后续启用 CPU 时，通过新版本设计评审后将 `cpu` 加入白名单；ResourceList 数据结构和算法接口无需重做。

实际用量 Provider：

- 当前生产集群和本地 Kind 都已有 metrics-server，第一版实现 `MetricsAPI` Provider，读取 `metrics.k8s.io/v1beta1`。
- Prometheus 作为后续可选 Provider，用于更长时间窗口和更稳定的历史查询，不作为第一版开发环境的硬依赖。
- Provider 通过小接口隔离，Controller 的配额计算不直接依赖 metrics-server 或 Prometheus 客户端实现。

自动驱逐策略：

- `eviction.enabled` 默认必须为 `false`，DepartmentQuota 的 `evictionPolicy` 默认必须为 `Deny`。
- Quota 回收、部门超过 effectiveQuota、普通内存利用率波动都不能直接触发驱逐。
- 只有全局 `eviction.enabled=true`、部门 `evictionPolicy=Allow` 且达到 `SevereNodePressure` 条件时，Controller 才可以进入驱逐流程。
- 驱逐必须使用 `policy/v1` Eviction API、遵守 PDB、限制每轮数量并设置 cooldown。
- 只允许选择受管业务 Namespace 中正在使用借用额度的 Pod；系统 Namespace 永远不参与。
- 该功能单独实现和测试。关闭时 Controller 不需要删除 Pod 的 RBAC 权限。

## 6. Pod 与 ResourcePool 的绑定

### 6.1 “部门 × ResourcePool”的含义

“部门 × ResourcePool”只是二维核算维度，不是要求创建新的部门，也不是当前方案要求拆 Namespace。

例如 department-a 同时部署 ARM 和 AMD 工作负载：

| 部门 | ResourcePool | 当前 limits.memory | 当前动态 Quota |
| --- | --- | --- | --- |
| department-a | arm64v8-kylin | 40Gi memory | 72Gi memory |
| department-a | amd64-kylin | 100Gi memory | 192Gi memory |

之所以必须分别核算，是因为 ARM 节点池的空闲内存不能承载只允许调度到 AMD 节点池的 Pod，反之亦然。如果只给 department-a 一个全局内存数字，可能出现“部门总量未超限，但目标架构节点池已经没有内存容量”的错误判断。

当前采用的部署方式是：

- 一个业务 Namespace 只绑定一个部门。
- 同一 Namespace 中可以同时存在面向不同 ResourcePool 的工作负载。
- 每个 Pod template 用 `quota.kgpp34.io/resource-pool` 声明自己使用的节点资源池。
- Controller 按 department 和 resource-pool 两个维度分别汇总。
- 不要求为了区分 ARM/AMD 而拆分 Namespace。

后文提到的“一个部门 × 一个 ResourcePool = 一个 Namespace”仅是追求 apiserver 强一致配额时的可选方案，不是当前选定方案。

### 6.2 绑定规则

不能只依靠自动解析复杂 affinity 来确定资源池，因为 affinity 可能含有：

- 多个 OR nodeSelectorTerms。
- `In`、`NotIn`、`Exists` 等组合表达式。
- 仅 preferred affinity。
- 可以同时匹配多个 ResourcePool 的表达式。

所有受管 Pod 和 Pod template 应显式声明：

```yaml
metadata:
  labels:
    department: department-a
    quota.kgpp34.io/resource-pool: arm64v8-kylin
```

Webhook 必须校验：

1. `quota.kgpp34.io/resource-pool` 指向存在且 Ready 的 ResourcePool。
2. Pod 使用硬性 `nodeSelector` 或 `requiredDuringSchedulingIgnoredDuringExecution` 被限定到对应节点池。
3. 只有 preferred affinity 时拒绝请求，因为它不能保证实际调度到目标资源池。
4. Pod 调度后，以实际 Node 所属 ResourcePool 为最终统计归属。
5. 声明资源池与实际节点池不一致时，设置 `PoolMismatch` 告警并按实际节点池计费。

第一版建议由业务显式填写 resource-pool 标签，Webhook 只校验，不自动猜测。后续可以增加可选 Mutating Webhook，根据 resource-pool 标签注入 nodeSelector，但不能在没有明确资源池时自行选择架构。

## 7. Pod 资源计算规则

### 7.1 Pod 生命周期

计入配额：

- Pending
- Running
- Unknown
- 已设置 deletionTimestamp 但尚未真正删除的 Pod

不计入配额：

- Succeeded
- Failed

终止中的 Pod 继续计入，避免 Deployment 滚动发布、Job 重试或快速删除重建期间出现短暂的双重透支。

### 7.2 Memory Limit

对每种资源分别计算：

```text
regularContainers = 所有普通容器资源之和
initContainers    = 所有 init container 中的最大值
podEffective      = max(regularContainers, initContainers) + podOverhead
```

上述 Pod 级算法只读取每个容器的 `limits.memory`。第一版只统计：

- `memory`

第一版不统计、不限制：

- 所有 requests（包括 `requests.memory`）
- `cpu`
- `ephemeral-storage`
- `hugepages-*`
- GPU 或其他 extended resource

后续版本可以扩展：

- `cpu`
- `ephemeral-storage`
- `hugepages-*`
- GPU 或其他 extended resource

将来增加 extended resource 时需单独评审其可超卖语义。

### 7.3 未声明资源的 Pod

平台部署的 Pod 已统一声明 CPU 和内存 requests/limits。第一版 Quota Webhook 只要求并校验 `limits.memory`；`requests.memory` 和 CPU 字段即使存在也不参与本 Controller 的配额准入判断。

不建议第一版通过 Mutating Webhook 隐式填充默认资源，因为默认值属于容量治理策略，需要业务部门确认。若后续确需默认值，应在 ElasticQuotaPolicy 中显式配置，并保证 mutation 幂等。

## 8. 实时统计设计

### 8.1 事件源

Controller Watch：

- Pod CREATE、UPDATE、DELETE。
- Node CREATE、UPDATE、DELETE。
- ResourcePool 变化。
- DepartmentQuota 变化。
- ElasticQuotaPolicy 变化。
- Namespace 部门标签变化。

Pod informer 建议建立索引：

- department
- resource-pool
- spec.nodeName
- namespace

### 8.2 Reconcile 模型

配额分配是集群级计算，不适合每个 DepartmentQuota 独立计算后分别修改共享池。

建议由 `ElasticQuotaPolicyReconciler` 作为全局规划器：

1. 所有相关事件映射到单例 `ElasticQuotaPolicy/default`。
2. Workqueue 自动合并短时间内的重复事件。
3. 每轮读取 informer cache 中的 Node、Pod、ResourcePool 和 DepartmentQuota。
4. 生成不可变计算快照。
5. 计算所有部门的 allocated 和 effective quota。
6. 使用 Status Patch 更新变化的对象。
7. 原子替换供 Webhook 读取的内存快照。

另外每 5 分钟执行一次全量 reconcile，用于修正 watch 丢失、历史数据和临时错误。

Controller 不需要为统计任务额外创建无限 goroutine。所有后台任务必须跟随 Manager Context 退出。

阶段 2 的实现先发布 capacity 和 allocated status，不提前计算阶段 3 的 effective quota。ResourcePool 的 allocated limits 包含该池中所有非终态 Pod；DepartmentQuota 只聚合显式纳管 Namespace，并始终以 Namespace 平台标签作为可信部门身份。

### 8.3 Status 写入抑制

为了避免频繁写入 apiserver：

- 只有数值或 Condition 实际变化时才 Patch Status。
- Observed Usage 可以按固定间隔写入，而不是每个采样点写入。
- Status 更新事件使用 predicate 过滤，避免触发自循环。
- Quantity 比较必须使用数值比较，不能只比较字符串格式。

## 9. 动态超卖算法

### 9.1 两层预算与初始比例

每个 ResourcePool 同时维护两个预算。

物理准入预算：

```text
physical = sum(ready schedulable node allocatable) - reserve
admissionBudget = physical × admissionRatio
```

名义额度预算：

```text
entitlementBudget = physical × entitlementRatio
```

含义：

- `admissionRatio` 控制当前实际已接纳的 aggregate `limits.memory` 可以达到物理内存的多少倍；这是 limit 超卖的硬上界。
- `entitlementRatio` 控制发给各部门的动态 Quota 总和可以是物理容量的多少倍。它实现的是“额度超卖”，不代表所有部门可以同时把额度用满。
- `admissionBudget` 是 Webhook 对整个 ResourcePool 执行的 aggregate `limits.memory` 实际接纳上限。
- `entitlementBudget` 是 Controller 计算各部门 effectiveQuota 时使用的名义额度池。
- 即使部门 effective quota 之和超过物理容量，Webhook 仍检查资源池总 allocated 是否超过 admissionBudget。
- requests 仍由 kube-scheduler 独立核算，本 Controller 不使用 requests 作为任何一个预算的输入。

第一版只管理内存，建议值：

| 比例 | Memory | 原因 |
| --- | --- | --- |
| admissionRatio | 1.5 | 已接纳的 limits.memory 总和最多达到可用物理内存的 1.5 倍 |
| entitlementRatio | 2.0 | 各部门名义动态额度之和允许达到可用物理内存的 2 倍 |

上线后可以根据 metrics-server/Prometheus 数据调整：

- 持续观察内存工作集、OOM、MemoryPressure、驱逐事件和 Pending Pod。
- entitlementRatio 只扩大名义额度，admissionRatio 仍限制当前 aggregate `limits.memory`。
- 比例必须在 Observe/Warn 阶段根据业务 limit/working-set 比率校准；无法证明安全时应降低 admissionRatio。
- admissionRatio 不影响 kube-scheduler 的 requests 调度判断，limit 超卖风险通过实际用量、OOM、MemoryPressure 和可选严重压力驱逐进行观测与保护。

### 9.2 历史基础 Quota 校验

第一版只对资源池中的 `limits.memory` 计算：

```text
sumBaseQuota = sum(all departments baseQuota.memory)
```

历史 baseQuota 不是物理保证，因此允许 `sumBaseQuota > physical`。出现这种情况时：

- ResourcePool 设置 `BaseQuotaOversubscribed=True`，用于暴露历史超卖程度。
- 所有部门的 effectiveQuota 仍不得低于各自 baseQuota。
- 可供动态借用的共享池按 0 计算，直到配置的 entitlementBudget 大于 sumBaseQuota。
- Webhook 继续用 admissionBudget 检查当前实际 aggregate `limits.memory`；达到资源池上限后拒绝后续正向增量。
- 不自动删除已有 Pod。

有效的名义额度预算定义为：

```text
effectiveEntitlementBudget = max(
    physical × entitlementRatio,
    sumBaseQuota
)
```

这使历史 baseQuota 能被完整表达，但不会把它误认为物理资源保证。`BaseQuotaOversubscribed` Condition 和指标必须明确展示由历史 baseQuota 隐含的实际超卖比例。

### 9.3 部门目标额度

对部门 i 的 `limits.memory`：

```text
utilizationTarget = allocatedMemoryLimits[i] / targetUtilization
growthTarget      = allocatedMemoryLimits[i] × (1 + growthHeadroomPercent)

desiredMemoryLimits[i] = max(
    baseQuotaMemory[i],
    utilizationTarget,
    growthTarget
)

desiredMemoryLimits[i] = min(
    desiredMemoryLimits[i],
    maxQuotaMemory[i]
)
```

例如 target utilization 为 80%，部门已经声明 64Gi `limits.memory`，则 utilizationTarget 为 80Gi。这样 Controller 会在部门真正触顶前预留增长空间。

### 9.4 单一 Limit 额度口径

第一版只有一组部门额度：`baseQuota.memory <= effectiveQuota.memory <= maxQuota.memory`，全部表示 aggregate `limits.memory`。不再维护 requests 额度，也不存在 request 与 limit 之间的联动换算。

limits 不参与 kube-scheduler 节点放置，因此其超卖比例必须独立于调度器 requests 评估。风险通过 DepartmentQuota maxQuota、ResourcePool admissionRatio、Observed Usage、Node MemoryPressure 和可选严重压力驱逐共同控制。

### 9.5 加权水位分配

分配步骤：

1. 为所有部门分配 baseQuota。
2. 计算共享池：`shared = max(0, effectiveEntitlementBudget - sumBaseQuota)`。当历史 baseQuota 已超过配置的 entitlementBudget 时，shared 为 0。
3. 计算每个部门的额外需求：`extraDemand = desired - baseQuota`。
4. 如果 extraDemand 总和不超过 shared，则全部满足。
5. 如果共享池不足，按 weight 使用 weighted max-min fairness 分配。
6. 已达到 maxQuota 的部门退出后续分配，剩余资源继续分给其他部门。
7. 第一版只对 memory 计算；算法实现保持 ResourceList 形式，为后续资源扩展保留接口。

算法必须满足以下不变量：

```text
baseQuota <= effectiveQuota <= maxQuota
sum(effectiveQuota) <= effectiveEntitlementBudget
effectiveQuota >= allocated，除非已进入明确的 OverQuota 状态
```

### 9.6 扩容与回收防抖

扩容规则：

- 使用率超过目标值时可以立即扩容。
- 单次扩容不超过 `maxIncreasePercent` 计算出的增量，但至少覆盖 `minIncrease.memory`；两者取较大值。
- 不得超过 DepartmentQuota maxQuota。

回收规则：

- allocated 和 observed usage 均持续低于阈值超过 `reclaimAfter`。
- 回收必须经过 cooldown。
- 单次回收不超过 `maxDecreasePercent`。
- 不得回收到 baseQuota 以下。
- 正常回收不得低于公平分配目标；若当前 allocated 已超过有效额度，则显式进入 OverQuota，不自动驱逐既有 Pod。

Node MemoryPressure 或监控内存利用率超过压力阈值时：

- 立即停止新增借用。
- Webhook 拒绝正向增量。
- 逐步将空闲的 effective quota 回收到 baseQuota。
- 默认不自动驱逐 Pod，不自动修改运行中 Pod 的 memory requests/limits。

如果 `eviction.enabled=true`，驱逐仍不能作为普通 Quota 回收手段。只有严重节点压力持续超过 sustainedFor 后才允许执行，并且必须满足第 5.3 节定义的限速、PDB 和业务 Namespace 约束。

压力恢复必须同时满足恢复阈值和 `recoveryWindow`，避免在阈值附近来回切换。

阶段 3 首先以 Node `MemoryPressure=True` 作为可用的强压力信号。节点恢复后仍在 `recoveryWindow` 内保持 `BorrowingBlocked=True`。基于 metrics-server/Prometheus 的 high/severe watermark 判定需要先完成 ObservedUsage 采集，在后续可观测性阶段接入；它不会改变严格准入口径仍为 `limits.memory`。

## 10. Admission Webhook 设计

### 10.1 Webhook 类型

第一版使用 Validating Admission Webhook。

需要覆盖：

- Pod
- Deployment
- StatefulSet
- DaemonSet
- ReplicaSet
- Job
- CronJob

工作负载对象的检查用于提前反馈；Pod 检查是最终兜底，防止用户直接创建 Pod 或使用自定义 Controller 绕过工作负载检查。

扩展性约束：

- Pod Webhook 是权威最终检查。以后即使增加创建 Pod 的自定义 Controller，只要它最终向受管 Namespace 创建 Pod，就不能绕过部门 Quota。
- Deployment、StatefulSet、Job 等工作负载级检查只负责让用户在提交上层对象时更早得到反馈。
- 工作负载资源预估通过内部 `Projector` 小接口实现，第一版注册 Kubernetes 内置工作负载 projector。
- 后续出现自定义 CRD 时，可以增加对应 projector；在没有 projector 之前，上层 CR 可能先被接受，但其创建的 Pod 仍会接受最终准入检查。
- 第一版不做一个接收任意 `unstructured.Unstructured` 的通用反射推断器，因为无法可靠知道任意 CRD 的副本和 Pod template 语义。

Kubebuilder 的 `create webhook` 主要面向自定义资源。内置 Pod、apps、batch 资源的 handler 需要基于 controller-runtime `admission.Handler` 手工注册。

### 10.2 准入流程

对于 CREATE：

1. 判断 Namespace 是否启用 quota enforcement。
2. 读取 Namespace 绑定的部门。
3. 校验对象或 Pod template 的 department 标签。
4. 校验 resource-pool 标签和硬性调度约束。
5. 校验每个普通容器和 init container 都声明正数 `limits.memory`，避免通过省略 limit 绕过额度。
6. 计算对象可能增加的 `limits.memory`；忽略 requests 和 CPU delta。
7. 检查部门 effective quota。
8. 检查部门 maxQuota。
9. 检查 ResourcePool admissionBudget。
10. 根据 Observe、Warn 或 Enforce 返回结果。

对于 UPDATE：

```text
delta = max(newProjectedResources - oldProjectedResources, 0)
```

只有 delta 大于 0 时执行额度检查。

始终允许：

- DELETE。
- 缩容。
- 降低 `limits.memory`；requests 和 CPU 增减不受本 Controller 限制。
- 修复 department、resource-pool 或调度约束。
- Status 子资源更新。

### 10.3 工作负载预估

Deployment、StatefulSet：

```text
projected = replicas × podTemplateResources
```

Job：

```text
projected = parallelism × podTemplateResources
```

CronJob 需要结合 `concurrencyPolicy` 和 Job template 计算保守上界。无法可靠计算时，工作负载级别只做标签和策略校验，最终由 Pod admission 负责资源准入。

DaemonSet 必须根据目标 ResourcePool 中符合其 nodeSelector/affinity 的节点数量计算，不使用固定 replicas。

工作负载预检查不是资源预留。Deployment 通过 admission 后，在其 Pod 被创建前，其他请求可能消耗额度，因此 Pod admission 仍有权拒绝后续 Pod。

### 10.4 Webhook 配置

建议配置：

```yaml
failurePolicy: Fail
sideEffects: None
timeoutSeconds: 2
matchPolicy: Equivalent
admissionReviewVersions:
  - v1
```

安全要求：

- 不能使用依赖 department 对象标签的 `objectSelector`，攻击者可以通过省略标签绕过。
- 使用管理员控制的 Namespace label 作为 `namespaceSelector`。
- Webhook 请求路径不得调用外部监控系统。
- Webhook 只读取本地 informer cache 和不可变准入快照。
- 快照未初始化、过期、部门未知或资源池未知时，Enforce 模式对正向增量 fail closed。
- Webhook 至少运行两个副本，并配置 PodDisruptionBudget 和跨节点反亲和。
- Webhook 自身 Namespace 不纳入该 Webhook 的 quota enforcement，避免循环依赖。
- 修改 Namespace 部门标签、CRD 配额和 Webhook 配置的权限只授予平台管理员。

默认部署使用 cert-manager 为 Webhook Service 签发证书并向 ValidatingWebhookConfiguration 注入 CA。Manager 以双副本运行；Controller 仍使用 leader election，而两个副本都可提供无副作用的准入读取服务。

### 10.5 拒绝信息

拒绝响应必须包含：

- department。
- ResourcePool。
- 资源名称。
- 当前 allocated。
- 本次 delta。
- effective quota。
- maxQuota 或 admissionBudget。
- 可执行的修复建议。

示例：

```text
department department-a exceeds memory quota in resource pool arm64v8-kylin:
allocatedLimits=70Gi, limitDelta=4Gi, effectiveQuota=72Gi;
reduce replicas or memory limits, or request a higher department maxQuota
```

## 11. 强一致性限制与部署模式

### 11.1 标签聚合模式的限制

自定义 Webhook 从 informer/cache 读取聚合数据时，多个并发请求可能同时看到相同的旧快照。

因此纯标签聚合模式可以做到：

- 秒级收敛。
- 正常情况下严格拒绝超限请求。
- 极端并发下可能出现小范围额度穿透。
- kube-scheduler 仍独立执行节点 requests 容量检查；它不能替代本 Controller 的 aggregate limits 配额检查。

不建议在 admission 请求中写 DepartmentQuota Status 作为“预占”。Admission 副作用会产生请求最终失败但额度已占用、Webhook 重试重复占用和多副本冲突等问题。

### 11.2 强一致模式

原生 Kubernetes ResourceQuota 的核算发生在 apiserver 内，但只按 Namespace 聚合，不能按 Pod label 或节点类型统计。

这里的“部门 × ResourcePool”是指某部门在某一种节点资源池中的单独账本。例如：department-a 的 ARM64/Kylin 配额和 department-a 的 AMD64/Kylin 配额是两个独立核算项。

如果未来要求任何并发下都不能超过这个二维核算项，可以把账本映射为独立 Namespace：

```text
一个部门 × 一个 ResourcePool = 一个独立 Namespace
```

Controller 为这些 Namespace 动态维护原生 ResourceQuota，Webhook 负责：

- 提前拒绝 Deployment 等工作负载对象。
- 校验部门和资源池绑定。
- 防止用户删除或修改平台管理的 ResourceQuota。

这种模式可以获得 apiserver 级别的并发配额一致性，但要求 department-a 的 ARM 和 AMD 工作负载进入不同 Namespace，会改变现有 Namespace 组织方式。

### 11.3 当前选择

- 当前确认一个业务 Namespace 绑定唯一部门，但不要求按 ResourcePool 拆分 Namespace。
- 当前采用标签聚合模式：同一业务 Namespace 中不同 Pod 可以分别使用 ARM 和 AMD ResourcePool，Controller 按 department + resource-pool 统计。
- 系统 Namespace 不带 enforcement/department 标签，完全不参与该模式。
- 通过并发压测记录自定义 Webhook 快照造成的最大穿透量。
- 如果未来把“任何并发下绝不穿透”升级为硬性要求，再评估按 ResourcePool 拆 Namespace或实现 scheduler/apiserver 扩展。

## 12. 故障与边界场景

| 场景 | 处理方式 |
| --- | --- |
| metrics-server 不可用 | 设置 MetricsDegraded，不影响 `limits.memory` 准入 |
| 未带 enforced 标签的系统 Namespace | 完全跳过部门统计和业务 Quota Webhook |
| ResourcePool 无 Ready Node | 停止新增，已有 Pod 继续计数 |
| Node 突然 NotReady | 容量移出新增预算，Node 上 Pod 继续计数 |
| baseQuota 总和超过物理容量 | 设置 BaseQuotaOversubscribed，停止动态借用，按 admissionBudget 限制实际新增 |
| allocated 大于新 effective quota | 设置 OverQuota，允许删除/缩容，拒绝新增 |
| 部门配置被删除但 Pod 仍存在 | 设置 OrphanedDepartment，Enforce 模式拒绝新增 |
| Node 同时匹配多个 Pool | 设置 SelectorOverlap，该 Node 不提供新增容量 |
| Pod 没有 Pool 标签 | 受管 Namespace 中拒绝 |
| Pod 只有 preferred affinity | 拒绝，因为不能保证目标节点池 |
| Webhook 快照未就绪或过期 | Enforce 模式对正向增量 fail closed |
| Controller leader 切换 | 新 leader 从 informer cache 全量重算，不依赖内存历史 |
| CRD Status 更新冲突 | 使用 Patch 和冲突重试，不覆盖 Spec |

## 13. 可观测性

### 13.1 Prometheus 指标

建议暴露：

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

避免把 pod name、UID 等高基数字段放入 Prometheus label。

### 13.2 Conditions

ResourcePool Conditions：

- Ready
- SelectorOverlap
- NoReadyNodes
- BaseQuotaOversubscribed
- AdmissionOverCapacity
- MemoryPressure

DepartmentQuota Conditions：

- Ready
- OverQuota
- OrphanedNamespace
- UnknownResourcePool
- PoolMismatch
- MetricsDegraded

ElasticQuotaPolicy Conditions：

- Ready
- SnapshotStale
- ConfigurationInvalid
- MetricsDegraded

### 13.3 Kubernetes Event

以下情况生成 Event，但必须限频：

- DepartmentQuota 首次进入 OverQuota。
- 借用额度被扩大或回收。
- Admission 请求被拒绝。
- Node 无法归类或匹配多个 ResourcePool。
- metrics-server 持续不可用。
- 自动驱逐开关启用、执行或因 PDB 被拒绝。

## 14. Kubernetes 1.23 兼容策略

当前本机工具环境：

```text
Go:          1.24.4
Kubebuilder: 4.6.0
默认 K8s:    1.33.0
```

Kubebuilder CLI 只负责生成脚手架，运行兼容性主要由 controller-runtime、client-go、使用的 API 和生成 manifest 决定。

以 Kubernetes 1.23 为明确兼容基线时，建议：

- controller-runtime `v0.11.2`。
- `k8s.io/api`、`k8s.io/apimachinery`、`k8s.io/client-go` 使用 `v0.23.x`。
- controller-gen `v0.8.x`。
- 使用 `apiextensions.k8s.io/v1`。
- 使用 `admissionregistration.k8s.io/v1`。
- 使用 `policy/v1` Eviction/PDB API；自动驱逐能力默认关闭，只有显式启用后才使用。
- CRD 使用结构化 OpenAPI schema。
- 不使用 ValidatingAdmissionPolicy、CEL matchConditions、Pod-level resources、原生 sidecar container 等后续版本能力。

推荐使用隔离的 Kubebuilder 3.3 二进制生成 1.23 基线工程，而不是覆盖本机 Kubebuilder 4.6。也可以用 4.6 生成后手工降级依赖和修改脚手架，但风险更高、工作量更大。

Kubernetes 1.23 已停止安全维护。本项目支持 1.23 是兼容性要求，不代表建议继续运行未维护的控制面版本。

## 15. Go 工程结构

建议使用标准 Kubebuilder 扁平结构和手工构造注入：

```text
.
├── api/
│   └── v1alpha1/
│       ├── resourcepool_types.go
│       ├── departmentquota_types.go
│       ├── elasticquotapolicy_types.go
│       ├── groupversion_info.go
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go
├── internal/
│   ├── controller/
│   │   └── policy_controller.go
│   ├── quota/
│   │   ├── allocator.go
│   │   ├── capacity.go
│   │   ├── classifier.go
│   │   ├── resource.go
│   │   └── snapshot.go
│   ├── metrics/
│   │   └── collector.go
│   └── webhook/
│       ├── workload_handler.go
│       ├── projector.go
│       └── response.go
├── config/
│   ├── crd/
│   ├── default/
│   ├── manager/
│   ├── rbac/
│   ├── samples/
│   └── webhook/
├── test/
│   └── e2e/
├── Dockerfile
├── Makefile
├── PROJECT
├── go.mod
└── go.sum
```

设计原则：

- `internal/quota` 中的资源计算和分配算法保持纯函数，不依赖 controller-runtime Client。
- 不创建泛化的 `util` 或 `helper` 包。
- 接口定义在消费方，仅对 metrics client、clock 或测试替身等确有需要的依赖抽取小接口。
- Manager 负责 Context 生命周期、leader election、cache 和 webhook server。
- Webhook 读取不可变 Snapshot，不与 Controller 共享可变 map。
- 所有 ResourceList 运算必须正确 DeepCopy `resource.Quantity`，避免引用和格式问题。

## 16. RBAC 与安全

Controller 需要：

- get/list/watch Node、Pod、Namespace。
- get/list/watch/patch/update 三类 CRD 及其 status。
- create/patch Event。
- get/list/watch metrics API；如果 metrics 功能启用。
- 管理 ResourceQuota；仅强一致模式需要。
- create `pods/eviction`；仅在构建并启用自动驱逐功能时授予，默认部署清单不授予。

默认关闭自动驱逐时，Controller 不需要：

- 删除普通业务 Pod。
- 修改业务 Deployment/StatefulSet。
- 读取 Secret 内容，证书挂载除外。
- 集群管理员级通配权限。

普通部门用户必须被 RBAC 禁止：

- 修改 Namespace 的部门绑定标签。
- 修改或删除 DepartmentQuota、ResourcePool、ElasticQuotaPolicy。
- 修改或删除平台管理的 ResourceQuota。
- 修改 ValidatingWebhookConfiguration。

## 17. 测试方案

### 17.1 单元测试

使用表驱动测试覆盖：

- 普通 container 资源求和。
- init container 最大值。
- Pod Overhead。
- Pending、Running、Succeeded、Failed、Terminating Pod。
- Node 到 ResourcePool 的唯一匹配、未匹配和重复匹配。
- baseQuota、maxQuota、headroom 计算。
- weighted max-min fairness。
- `limits.memory` 超过 effectiveQuota。
- requests 或 CPU limits 发生变化时不会触发配额拒绝。
- 容量缩小后 allocated 大于 effective quota。
- Quantity 精度、单位和溢出边界。

### 17.2 Envtest 集成测试

- CRD schema 和 validation。
- Pod、Deployment、Job admission。
- CREATE、UPDATE、缩容和 DELETE 行为。
- Observe、Warn、Enforce 三种模式。
- Snapshot 未就绪和过期。
- Status Patch 冲突。
- Controller 重启后的全量重算。

### 17.3 E2E 测试

至少覆盖 Kubernetes 1.23 和一个较新版本：

- 人工给 Kind 节点增加 ARM64/Kylin、AMD64/Kylin 标签。
- 部门在两个资源池中的独立统计。
- 高利用部门获得借用额度。
- 低利用部门经过 reclaimAfter 后归还额度。
- 超限 Pod 和 Deployment 被拒绝。
- 删除、缩容可以恢复 OverQuota。
- Node cordon、NotReady 和删除。
- metrics-server 故障不影响 `limits.memory` 准入。
- 自动驱逐关闭时不会创建 Eviction；开启时遵守 PDB、限速和 cooldown。
- Webhook 双副本滚动升级。

#### 本地 Kind 开发环境

本地开发以现有 Kind + metrics-server 为主：

- 使用真实 `metrics.k8s.io/v1beta1` 验证 MetricsAPI Provider。
- Kind 节点实际只有一种 CPU 架构，因此通过测试标签模拟 ARM64/Kylin 和 AMD64/Kylin ResourcePool；测试重点是分类、统计和准入，不验证不同 CPU 指令集的真实执行。
- 单节点 Kind 无法完整验证跨节点容量分配、PDB 和反亲和；相关场景使用 envtest/fake client 单元测试，并在后续多节点 Kind 或集成集群补充。
- 本地不要求部署 Prometheus。Prometheus Provider 使用接口桩和单元测试，等生产联调阶段再验证真实查询。

### 17.4 并发与稳定性测试

- `go test -race ./...`。
- 并发提交大量 Pod，测量标签聚合模式的最大额度穿透量。
- Webhook P99 延迟和超时率。
- 10 万 Pod 规模下的 reconcile 时间和内存使用。
- informer 重连和 apiserver 短暂故障。
- leader 切换期间的准入行为。

## 18. 实施阶段与验收标准

### 阶段 0：需求决策

交付：

- 固化已确认的 Namespace 纳管、标签聚合、baseQuota、驱逐开关和 MetricsAPI 决策。
- 收集各部门在各 ResourcePool 中的 baseQuota 和 maxQuota 初始值。
- 确认第一批开启 Observe 模式的业务 Namespace。

验收：不存在影响 API 结构的未决问题。

### 阶段 1：工程与 CRD

交付：

- Kubebuilder 工程。
- 三个 CRD、samples、RBAC 和生成代码。
- 1.23 兼容依赖锁定。
- ResourceList 基础运算。

验收：

- `make generate`、`make manifests` 可重复执行且无 diff。
- 单元测试通过。
- CRD 可安装到 Kubernetes 1.23 测试环境。

### 阶段 2：资源统计

交付：

- Node 分类和容量统计。
- Pod `limits.memory` 聚合。
- DepartmentQuota 和 ResourcePool Status。
- 全量重算与事件驱动 reconcile。

验收：

- Pod 创建、删除和调度迁移后状态在目标时间内收敛。
- Controller 重启后可以从集群对象重建全部状态。

### 阶段 3：动态分配

交付：

- 两层预算。
- 加权水位算法。
- cooldown、headroom、scale step 和 reclaimAfter。
- 压力状态处理。

验收：

- 所有算法不变量由单元测试覆盖。
- effectiveQuota 不低于 baseQuota、不高于 maxQuota。
- 资源池有效额度总和不超过 effectiveEntitlementBudget。

### 阶段 4：Webhook

交付：

- Pod 最终准入。
- 内置工作负载提前检查。
- Namespace/department 和 Pool/affinity 校验。
- Observe、Warn、Enforce。
- 证书和 HA 部署配置。

验收：

- 超限正向请求被拒绝。
- 删除、缩容和降低资源始终允许。
- 只增加 requests 或 CPU limits 的请求不会被本 Controller 以 Quota 原因拒绝。
- 省略 department/resource-pool 标签不能绕过。
- Webhook 延迟满足约定 SLO。

### 阶段 5：可观测性和上线

交付：

- Prometheus 指标和告警规则。
- Grafana Dashboard。
- 故障运行手册。
- E2E 和并发压测报告。
- 默认关闭的自动驱逐开关、保护条件和审计指标。

上线顺序：

1. `Observe` 模式运行并核对统计结果。
2. `Warn` 模式收集业务影响。
3. 修正历史工作负载标签和资源声明。
4. 小范围 Namespace 开启 `Enforce`。
5. 逐步扩大到全部受管 Namespace。

## 19. 第一版默认行为与明确不做的内容

- `eviction.enabled=false` 时不驱逐业务 Pod；即使启用，也只在严重节点压力下按保护规则执行，不能用于普通 Quota 回收。
- 不自动降低运行中 Pod 的 memory requests/limits。
- 不自动把 Pod 修改为 BestEffort。
- 不实现自定义 scheduler plugin。
- 不实现跨集群统一额度。
- 不使用历史预测模型或机器学习分配额度。
- 不为任意自定义 CRD 通用推断其未来创建的 Pod 数量。

任意 CRD 的通用资源推断等能力可以在第一版稳定后单独设计，不能与基础配额准入同时引入。

## 20. 已确认决策与编码前剩余输入

根据本轮需求确认，以下决策已经确定：

1. 一个受管业务 Namespace 由平台绑定到唯一部门。
2. 只有带 `quota.kgpp34.io/enforced=true` 的业务 Namespace 纳管；`kube-system`、`koordinator-system` 等系统 Namespace 默认不纳管。
3. 当前不按 ResourcePool 拆 Namespace；同一部门的 ARM/AMD 使用量通过 department + resource-pool 二维账本分别统计。
4. 历史“最小 quota”定义为 `baseQuota`：它是动态 Quota 不能回收低于的部门基础上限，不是物理资源保证。
5. 业务 Pod 已统一声明 CPU/内存 requests 和 limits；第一版只核算和限制 `limits.memory`，不限制 requests。
6. 自动驱逐默认关闭，但提供显式配置开关；启用后也只允许在严重节点压力下受控执行。
7. 当前没有其他创建 Pod 的自定义 Controller；Pod 最终准入天然覆盖未来 Controller，并为上层 CRD projector 保留扩展点。
8. 生产已有 metrics-server 和 Prometheus；第一版使用 MetricsAPI，Prometheus 为后续可选 Provider。
9. 本地开发使用现有单节点 Kind + metrics-server，不要求本地安装 Prometheus。

开始编码前还需要提供或确认：

1. 各部门在 ARM64/Kylin、AMD64/Kylin ResourcePool 中的 `baseQuota.memory` 和 `maxQuota.memory` 初始值，均表示 aggregate `limits.memory`。
2. 是否接受第一版 limit 超卖初始比例：admissionRatio 为 1.5，entitlementRatio 为 2.0。
3. 第一批进入 Observe 模式的业务 Namespace 列表或标签规则。
4. 第一批允许严重节点压力驱逐的部门列表；未显式配置 `evictionPolicy=Allow` 的部门保持 Deny。

这些输入不会改变整体架构，但会决定 samples、默认值和验收用例。

## 21. 参考资料

- [Kubernetes Resource Quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/)
- [Kubernetes Resource Management for Pods and Containers](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/)
- [Kubernetes Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)
- [Kubernetes 1.23 API Reference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.23/)
- [Kubernetes Deprecated API Migration Guide](https://kubernetes.io/docs/reference/using-api/deprecation-guide/)
- [controller-runtime version compatibility](https://github.com/kubernetes-sigs/controller-runtime)
- [Kubebuilder 3.3 release](https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v3.3.0)
