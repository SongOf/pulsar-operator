# 运维功能范围

本文定义 pulsar-operator 要接管的 Pulsar / BookKeeper 集群运维功能，并标出当前实现状态。

状态：✅ 已实现并经过测试　🟡 部分实现　⬜ 规划中

## 设计原则

1. **声明式**：所有运维操作都是修改 `PulsarCluster` / `BookKeeperCluster` 的 `spec`，控制器负责收敛。控制台、kubectl、GitOps 走同一条路径。
2. **安全优先于速度**：可能丢数据的操作（bookie 缩容、缩盘、改 ZooKeeper 成员数）要么按安全流程逐步执行，要么拒绝并在 `status.conditions` 说明原因。
3. **状态可观测**：每个组件的副本、就绪数、镜像、滚动进度，每个 bookie 的 ID 与 PVC 容量，以及进行中的下线流程，都写在 `status` 中。

## 资源模型

| CRD | 作用 |
| --- | --- |
| `PulsarCluster`（短名 `psc`） | 完整集群：ZooKeeper、BookKeeper、Broker、Proxy |
| `BookKeeperCluster`（短名 `bkc`） | BookKeeper 集群。PulsarCluster 会创建一个同名的子资源；也可单独使用，连接已有 ZooKeeper |

## 1. 集群生命周期

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 创建集群 | ✅ | 按依赖顺序启动：ZooKeeper → BookKeeper 元数据初始化 → bookie → Pulsar 集群元数据初始化 → broker → proxy。前一步就绪才开始下一步 |
| 元数据初始化 | ✅ | `initnewcluster` 与 `initialize-cluster-metadata` 由 Job 执行，可重复执行 |
| 删除集群 | ✅ | 级联删除工作负载；bookie PVC 按 `storage.reclaimPolicy`（Retain/Delete）保留或删除 |
| 暂停调谐 | ✅ | `spec.paused: true`，用于人工维护 |
| 集群状态汇总 | ✅ | `status.phase`：Initializing / Running / Updating / Scaling / Decommissioning / Degraded / Failed |
| 多集群 / geo-replication | ⬜ | 跨集群复制、全局 configuration store |

## 2. Pulsar（Broker / Proxy）

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| Broker 扩容 / 缩容 | ✅ | broker 无状态，缩容时由其他 broker 接管 topic |
| Proxy 启用 / 停用 / 扩缩容 | ✅ | Service 类型 ClusterIP / NodePort / LoadBalancer |
| 滚动重启 | ✅ | 设置 `restartedAt`，相当于 `kubectl rollout restart`；控制台一键操作 |
| 健康检查 | ✅ | broker 就绪探针为 `/admin/v2/brokers/ready` |
| PodDisruptionBudget | ✅ | 每次最多中断 1 个 |
| 反亲和 / nodeSelector / tolerations | ✅ | `pod.antiAffinity`：None / Preferred（默认）/ Required |
| Broker 优雅下线（先卸载 bundle） | 🟡 | 依赖 broker 自身的关闭流程；后续计划在缩容前主动执行 `unload` |
| 租户 / 命名空间 / Topic 管理 | ⬜ | 规划为独立 CRD（PulsarTenant、PulsarNamespace、PulsarTopic） |
| Functions worker | ⬜ | 当前固定关闭 |

## 3. BookKeeper

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 创建 bookie 集群 | ✅ | StatefulSet，bookie ID 为 Pod FQDN，重建后 ID 保持不变 |
| 扩容 | ✅ | 新 bookie 自动注册，并创建自己的 PVC |
| **安全缩容（decommission）** | ✅ | 控制台上每次操作只下线一个 bookie（"Decommission one bookie" 按钮）。正在下线、集群未全部就绪、已到最小副本数时按钮不可用。确认框会写明要下线的 bookie ID 和具体步骤 |
| 缩容流程 | ✅ | 每次下线一个，从序号最大的开始：①停止该 bookie；②执行 `decommissionbookie`，等待 autorecovery 把它的 ledger 复制到其他 bookie 并删除 cookie；③删除它的 PVC；④集群全部就绪后再处理下一个。失败时每 5 分钟重试，并发出事件 |
| 缩容下限保护 | ✅ | `minReplicas`（默认 3）。低于下限时停在下限，并报告 `ScaleInBlocked` |
| AutoRecovery | ✅ | 默认开启，可以关闭或调整副本数 |
| 下线进度可视化 | ✅ | `status.decommission`：bookie ID、阶段、消息、开始时间 |
| 机架感知 / 跨可用区放置 | ⬜ | 规划把节点拓扑写入 bookie 的 rack 信息 |
| 故障 bookie 自动替换 | ⬜ | 目前由 autorecovery 补齐副本，坏盘需要人工删除 PVC |

## 4. 磁盘管理（PV / PVC）

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| journal / ledger 独立 PVC | ✅ | 每个 bookie 各一个 journal PVC 和一个 ledger PVC，可分别指定大小和 StorageClass |
| ZooKeeper 数据 PVC | ✅ | |
| 在线扩容 | ✅ | StorageClass 允许扩容时，直接修改现有 PVC；同时用孤立删除（orphan）的方式重建 StatefulSet 以更新模板，Pod 不重启 |
| 不支持扩容时的处理 | ✅ | 报告 `StorageResized=False / ExpansionNotSupported`；新 bookie 使用新尺寸，现有 PVC 保持原尺寸 |
| 拒绝缩盘、拒绝修改 StorageClass | ✅ | 报告 `SpecRejected` 并给出原因，现有资源不受影响 |
| PVC 状态展示 | ✅ | 每个 PVC 的申请容量、实际容量、绑定状态、是否正在扩容 |
| 删除时的保留策略 | ✅ | `reclaimPolicy`：Retain（默认）或 Delete |
| 磁盘使用率告警 / 自动扩盘 | ⬜ | 需要接入 bookie 指标 |
| 更换 StorageClass（数据迁移） | ⬜ | 需要逐个下线 bookie 后重建，可以复用现有的 decommission 流程实现 |

## 5. 配置管理

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 各组件配置覆盖 | ✅ | `config` 的键值对写入 broker.conf / bookkeeper.conf / zookeeper.conf / proxy.conf，新增和覆盖都支持 |
| 配置变更自动滚动重启 | ✅ | 配置哈希写在 Pod 模板上，变更后逐个重启 |
| 受保护的配置项 | ✅ | 集群名、元数据地址、端口、bookie 目录等由 operator 管理，用户不能覆盖 |
| JVM 内存 | ✅ | `jvm.heap`、`jvm.directMemory`、`jvm.extraOptions` |
| 资源 requests / limits | ✅ | |
| 动态配置（不重启生效） | ⬜ | 规划通过 `pulsar-admin brokers update-dynamic-config` 下发 |
| 认证 / 授权 / TLS | ⬜ | JWT、mTLS、cert-manager 集成 |

## 6. 升级

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 镜像滚动升级 | ✅ | 按顺序升级：ZooKeeper → bookie → broker → proxy，每个组件全部换上新镜像并就绪后才升级下一个 |
| 单组件升级 | ✅ | 每个组件都可以设置 `image` 覆盖集群镜像，顺序约束同样生效；控制台上可以单独升级某个组件，也可以"恢复使用集群镜像" |
| 升级进度展示 | ✅ | 控制台按升级顺序显示每个组件的状态：已是最新 / 滚动中 x/y / 等待前一个组件 |
| 升级前检查 / 回滚 | ⬜ | |

## 7. ZooKeeper

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 创建集群 | ✅ | myid 由主机名推导 |
| 修改成员数 | 🟡 | 当前**禁止**修改，会报告 `SpecRejected`，因为需要 dynamic reconfig；计划后续实现 |
| 备份 / 恢复 | ⬜ | |

## 8. 控制台

| 功能 | 状态 |
| --- | --- |
| 集群列表与状态、需要处理的异常 condition | ✅ |
| 创建集群（表单） | ✅ |
| 组件扩缩容、滚动重启、启停 proxy | ✅ |
| bookie 与 PVC 明细、下线进度 | ✅ |
| 磁盘扩容、保留策略 | ✅ |
| 各组件配置编辑（键值表） | ✅ |
| 镜像升级：整个集群或单个组件，带进度展示 | ✅ |
| bookie 单步下线 | ✅ |
| Pod 列表、事件 | ✅ |
| 删除集群（输入名称确认） | ✅ |
| 访问令牌（`CONSOLE_TOKEN`） | ✅ |
| 监控大盘（见第 9 节） | ✅ |
| 租户 / Topic 管理 | ⬜ |

## 9. 监控

| 功能 | 状态 | 说明 |
| --- | --- | --- |
| 指标采集 | ✅ | `spec.monitoring.enabled` 会为集群部署一个 Prometheus，通过 Pod 自动发现抓取 broker、proxy、bookie 和 ZooKeeper 的指标 |
| 持久化 | ✅ | 数据存在 PVC 上（默认 20Gi），operator 或 Prometheus 重启后都不会丢失，已实测 Prometheus Pod 重建后历史数据完整 |
| 保留策略 | ✅ | 同时按时间（默认 15 天）和容量保留，容量默认为磁盘的 80%，最旧的数据先删除，磁盘不会被写满 |
| 长期存储 | ✅ | `remoteWrite` 可以同时写入 VictoriaMetrics、Thanos 或 Mimir；控制台查询时用 `prometheusURL` 指向这些长期存储 |
| 接入已有 Prometheus | ✅ | 设置 `prometheusURL` 后 operator 不再部署 Prometheus；`config/monitoring/podmonitor.yaml` 是 Prometheus Operator 的抓取配置 |
| 标准抓取注解 | ✅ | 每个组件的 Pod 都带有 `prometheus.io/scrape|port|path` |
| 控制台监控大盘 | ✅ | 指标卡片（消息速率、吞吐、积压、存储量、topic 数、可写 bookie 数），时序图（消息速率、吞吐、积压、存储量、每个 bookie 的磁盘使用率、bookie 写入吞吐、bookie 写入 p99 延迟、ZooKeeper 延迟），Pod 表（采集状态、堆内存、CPU）。时间范围 15m 到 7d，每张图都可以切换为表格 |
| 告警规则 / Alertmanager | ⬜ | 例如 bookie 变为只读、磁盘使用率超过 85%、积压持续增长 |
| Grafana 面板 | ⬜ | |
| Kubernetes 事件（下线开始、完成、失败，扩盘） | ✅ | |
