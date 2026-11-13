# Pulsar 设计与实现

`cmd/pulsar/main.go` 创建 runtime Scheme，注册 Kubernetes 内置类型和 `api/pulsar/v1alpha1`，再创建 Manager。Manager 统一提供缓存客户端、控制器生命周期、健康检查和 leader election；Pulsar 与 BookKeeper 控制器、Console HTTP 服务注册到同一进程。

`PulsarReconciler` 从 PulsarCluster 的期望状态依次协调 ZooKeeper、同名 BookKeeperCluster、初始化 Job、Broker 与 Proxy。同名子资源具有父资源所有权；BookKeeper 控制器独立处理 bookie 发现、扩容、逐个下线、PVC 策略和状态更新。详细功能状态见[运维范围](operator-scope.md)。

| 代码 | 责任 |
| --- | --- |
| `api/pulsar/v1alpha1` | CR spec/status、默认值和生成标记 |
| `internal/pulsar/controller` | 资源读取、比较、应用与状态机 |
| `internal/pulsar/resources` | 将 CR 转换成 StatefulSet、Deployment、Job、Service 等资源 |
| `internal/pulsar/httpapi` | Bearer Token 校验、查询、带 UID/resourceVersion 前置条件的变更 |
| `internal/pulsar/metrics` | 获取与聚合监控数据 |
| `internal/pulsar/audit` | SQLite 审计、请求关联、写入意图与结果、敏感值脱敏 |
| `config/pulsar` | CRD、控制器权限、进程配置与 CR 示例 |

控制台变更写入 CR，由控制器继续调谐。Storage 通过已验证的 StorageClass/池契约观察 Pulsar PVC，不能越过 Pulsar 的所有权直接释放或扩容这些卷。公用前端与 Gateway 是组合层，见[整体架构](../../../docs/design/architecture.md)。
