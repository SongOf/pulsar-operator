# Pulsar Operator

面向 Kubernetes 的 Pulsar 集群与本地磁盘管理项目，提供应用运维、存储管理、节点执行和统一 Web 控制台。

| 模块 | 项目功能 |
| --- | --- |
| [Pulsar](modules/pulsar/README.md) | 声明式部署 ZooKeeper、BookKeeper、Broker 和 Proxy；集群状态、滚动更新、扩缩容、Bookie 下线与存储扩容；用户操作及控制器关键动作审计。 |
| [Storage](modules/storage/README.md) | 磁盘与存储池库存、接管计划和审批、PVC 管理、操作审计，以及 Pulsar 存储配置规划。 |
| [Agent](modules/agent/README.md) | 每节点发现物理盘并上报；按批准的 DiskPool 核验设备身份、准备单盘 LVM VG 和 StorageClass。 |
| [公用前端](web/README.md) | 提供 Pulsar 和 Storage 两个页面入口，分别连接各模块 API，保留独立的访问凭证。 |

Pulsar 声明应用需要的卷，Storage 管理磁盘资源和使用计划，Agent 在目标节点执行准备工作。OpenEBS LVM LocalPV CSI 根据 PVC 动态创建业务卷并完成挂载与扩容；三个后端通过 Kubernetes 资源协作。

项目支持独立运行 Pulsar、组合 Storage 与 Agent，以及通过统一网关使用全部模块。Storage 还提供本地模拟模式，便于体验磁盘、存储池和卷管理流程。

本地 LVM/PV 不提供跨节点副本；Storage 和 Pulsar 当前分别使用单副本服务与独立持久卷保存 SQLite 状态和审计。真实部署的适用范围与既有验收记录见[项目文档](docs/README.md)。
