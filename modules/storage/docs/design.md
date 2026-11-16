# Storage 设计与实现

```mermaid
flowchart LR
  UI[中文管理页面] --> API[管理 API]
  API --> CR[PhysicalDisk / DiskPool CRD]
  CR --> OP[每节点 Operator DaemonSet]
  OP --> VG[一盘一 LVM Volume Group]
  OP --> BENCH[临时 LV 单盘压测]
  BENCH --> REVIEW[人工分配审核]
  REVIEW --> SC[StorageClass]
  API --> PVC[业务 PVC]
  PVC --> CSI[OpenEBS LVM LocalPV CSI]
  SC --> CSI
  CSI --> LV[动态 LV / 文件系统]
  VG --> LV
  LV --> POD[kubelet 挂载到 Pod]
```

本项目实现磁盘发现、审批、VG 纳管、StorageClass 生成和管理界面；动态创建 LV、绑定 PV、格式化、挂载、卸载与扩容由 **OpenEBS LVM LocalPV CSI** 完成。没有重新实现一套未经验证的 CSI 协议。

## 节点接入与磁盘准入

节点清单来自 Kubernetes Node，未启用 Agent、尚无 PhysicalDisk 的节点也会显示。统一 React 控制台的物理节点详情通过 Storage API 查询节点、Agent Pod 与该节点磁盘。部署按钮检查预装的 `diskfleet-system/diskfleet-agent` DaemonSet、节点健康与选择条件，携带 Node UID/resourceVersion 更新 `storage.diskfleet.io/enabled=true`；返回请求已接受，不代表 Pod 已运行。它不安装 CSI、不批准磁盘，也不改变 `ENABLE_DISK_WRITES`。

Agent 在成功扫描时发布 `PhysicalDisk.status.lastObservedTime`、`validUntil` 与 `nodeUID`。有效期覆盖一个自动调谐间隔及其最大 20% 抖动，默认 30 秒间隔对应 36 秒；扫描失败不续期，未再次观察到的盘会标为 Offline。旧 Agent 未上报这些字段时，库存仍可查看，但不能用于新的接管批准。

生成计划和创建 DiskPool 都重新读取节点、Agent 与磁盘，要求物理节点已确认运行 Linux、Ready、未删除/封锁且无压力，Agent Pod 正在运行并就绪，磁盘 Available、无保护原因且未被认领，设备与 Node 身份一致，成功扫描时间合法且未超过 `validUntil`。计划固定 Node/PhysicalDisk UID 和设备身份；同名重建或计划期间状态变化会拒绝创建。已有相同请求的幂等恢复不会重新初始化设备。前端显示不可操作原因并按有效期禁用按钮，服务端始终执行最终校验。

默认使用厚置备 `thinProvision: "no"`，`WaitForFirstConsumer`、`ReadWriteOnce`、`Retain` 和节点拓扑限制。只有消费 PVC 的 Pod 出现后才会绑定卷；创建 PVC 后看到 Pending 是正常状态。`fsType=ext4`，禁止缩容。普通 LVM 和本地 PV 不提供磁盘冗余或跨机器副本。

**LVM Physical Volume 与 Kubernetes PersistentVolume 是两种不同的 PV。** 原始物理盘加入 VG 后不需要挂到 `/mnt`，CSI 会在业务 LV 上创建文件系统并通过 kubelet 挂载。详见 [物理机接盘手册](physical-disks.md)。

## 单盘压测与分配审核

`spec.approved` 只批准设备初始化。Agent 成功创建 VG 后运行固定 profile 的临时 LV 压测，持久记录池/节点/设备身份、runID、四项顺序/随机读写指标和清理结果；临时 LV 清理完成并验证后，池停在 AwaitingReview，尚不发布 StorageClass。界面打开完整结果，由人工决定是否适合业务；压测成功本身不代表性能达标。

人工审核单独写入 `spec.allocationApproval`，固定 poolUID、runID、resultDigest，带服务端审核时间和备注。API 先保存审计意图，使用 UID/resourceVersion 原子更新，再记录结果；结果或对象变化会拒绝旧请求。拒绝后允许重新审核，通过后不可撤销或改写，因为已经发布的 StorageClass 不能安全撤回。Agent 校验匹配的 Approved 决定后才发布 StorageClass 并进入 Ready。新卷和 Fleet 规划均要求完整有效审批，提交新卷时实时重读池、Node UID 和实际 StorageClass。

旧 Ready 池没有报告时展示 LegacyUnreviewed，不能新分配，也不自动重测、删除 StorageClass 或更改已有卷。旧卷的观察、扩容和 Retain 恢复继续依赖原所有权和绑定校验。CSI 不读取 DiskPool 审批；延迟发布项目管理的精确 StorageClass 是正常创建路径的门禁，管理员仍需限制外部或宽泛 vgpattern 类选中未审核 VG。

模拟模式不运行 fio、不打开主机设备。新池生成明确标识的模拟成功报告等待人工审核，预置演示池带模拟通过记录，以便演示卷流程。所有结果经 SQLite 保存，重启不会丢失审核状态。

## 容量与一致性边界

- 每节点一个 Operator，按 `storage.diskfleet.io/node` 标签读取本节点资源，并再次核对 spec。超过 63 字符的节点名使用标签哈希，spec 仍保存原名；StorageClass 读取真实 hostname 标签作为拓扑。磁盘操作串行加文件锁，互相独立的节点并行工作。
- Kubernetes REST 列表采用分页；调谐间隔默认 30 秒并加入随机抖动；状态没有变化时不写入 API Server。
- 管理页分页、搜索与批量计划；每盘一个 VG，避免某盘故障影响同一 VG 内其他物理盘。
- 模拟模式容量预留在 SQLite 事务内完成，并发分配不能超配。真实厚置备的物理容量以 LVM/CSI 分配结果为准，管理 API 显示值可能滞后，PVC 可能因并发耗尽而 Pending。
- **尚未证明支持数万节点。** 当前管理 API 对池与卷每 30 秒缓存全量对象，节点/磁盘准入查询重新读取库存，单副本 SQLite 保存计划/审计，每盘一个 StorageClass；这些是扩大规模前要替换的部分。建议改为 watch 驱动索引、按机房分片、共享存储等级的 StorageClass，以及持久审计队列，并实测 API Server/etcd 负载。
- API 使用独立的 `diskfleet-api-state` PVC 保存计划、审计和释放记录；Deployment 固定单副本、使用 `Recreate` 更新。Pod 更换后复用同一状态卷，更新期间 API 暂时不可用。状态卷需要预先可用的基础设施 StorageClass 或静态 PV，不依赖尚待 API 初始化的 DiskFleet 池。API 暂不支持多副本。
- 当前提供设备可用性/容量、一次单盘压测结果与操作审计；SMART、温度、介质寿命、故障率时间序列应接 Prometheus/node-exporter/smartctl-exporter。没有实现故障盘自动迁移、跨节点复制、自动 RAID 或多租户权限隔离。

## 实现位置与接口

与同仓库 Pulsar Operator 的跨节点 StorageClass、只读 PVC 观察、配置生成器和真实验收入口见 [Pulsar 集成说明](pulsar-integration.md)。Pulsar 源码基于用户指定的 **SongOf/pulsar-operator** `3c042c5`。统一 Gateway 的 `/` 为 Pulsar 控制台，`/storage/` 为本模块控制台，两个模块各自验证访问凭证。也可直接访问本模块服务的 `/`；两种部署方式共用同一套前端文件。

| 位置 | 内容 |
|---|---|
| `../../cmd/agent`、`../../internal/agent` | Go 节点发现、CRD 调谐、StorageClass、安全生命周期 |
| `../../api/storage/v1alpha1` | Storage / Agent 共用的 Kubernetes 资源类型与设备身份 |
| `../../internal/storage/cluster` | Kubernetes 库存、DiskPool/PVC 管理与归属校验 |
| `../../cmd/agent` 的 `--discover` | Go Agent 的只读磁盘发现入口；先运行 `make build-agent` |
| `../../internal/kubeconfig` | Agent / Storage 共用的 ServiceAccount / 显式 TLS 连接配置 |
| `../../internal/storage/store` | SQLite 模拟容量事务、本地审计/计划、释放意图 |
| `../../internal/storage/fleet` / `../../cmd/storage-plan` | 只读 Pulsar 存储规划与 CLI |
| `../../cmd/storage-api` / `../../internal/storage/httpapi` | Go HTTP 入口、路由、鉴权和参数校验 |
| `../../../web/src/StorageWorkspace.jsx` | 与 Pulsar 共用 React 外壳的存储页面；独立 API 凭证 |
| `../../config/storage/` | 存储 CRD、API RBAC/Deployment、示例 |
| `../../config/agent/` | 独立 Go Agent DaemonSet 与 RBAC |
| `../../internal/storage/**/*_test.go` | HTTP 协议、并发、恢复、规划兼容性测试 |

接口字段见 [API 契约](api.md)。外部监听需配置 `API_TOKEN`，例如 `API_HOST=0.0.0.0`；默认只监听回环地址。服务应放在 TLS 代理之后，不应直接作为公网服务器使用。
