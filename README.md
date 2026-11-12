# pulsar-operator

同一仓库管理 **Apache Pulsar / BookKeeper 集群、物理磁盘、动态本地 PV 与管理控制台**。基于用户指定的 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator/tree/3c042c5) 源码快照，将磁盘管理实现作为 `modules/storage` 集成；保留原有 Go API 与控制器结构。

```text
物理盘 → LVM PV/VG → OpenEBS LVM CSI → LV / Kubernetes PV / PVC → Pulsar Pod
                 ↑                         ↑
          Storage 节点 Agent         Pulsar / BookKeeper 控制器
                 └──── 同仓库、统一部署与管理入口 ────┘
```

磁盘 Agent 负责发现、审批与一盘一 VG；CSI 负责逻辑卷创建、格式化、挂载与扩容；Pulsar 控制器负责组件、业务 PVC 和应用生命周期。Pulsar 管理的卷在存储页面只读，扩容通过父 `PulsarCluster` 提交。

## 文档入口

| 文档 | 内容 |
|---|---|
| [架构设计](docs/architecture.md) | 模块关系、所有权、控制流、磁盘到 Pod 的数据路径、规模边界 |
| [项目设计](docs/project-design.md) | 目录、接口、状态机、并发与安全设计、开发与测试策略 |
| [使用指南](docs/user-guide.md) | 本地演示、构建、统一部署、接盘、创建 Pulsar、扩容与验收 |
| [149 / 150 两节点部署](docs/two-node-guide.md) | 已有 Kubernetes 的检查、镜像、管理面、CSI、真实池和两节点 Pulsar 联调 |
| [两节点共享数据盘联调](docs/shared-data-disk-lab.md) | 本次每节点一块 100Gi 数据盘的独立配置、限制与实际验收入口 |
| [系统盘目录联调](docs/system-disk-lab.md) | 无数据盘时的目录存储替代方案，无磁盘配额与 CSI 扩容 |
| [物理盘操作手册](modules/storage/docs/physical-disks.md) | SAS/SATA/NVMe 接入、Linux 发现、by-id、审批、挂载和故障处理 |
| [验证报告](docs/validation.md) | 实际构建、两节点数据盘部署、消息持久化、扩容与验证边界 |
| [审查修复说明](docs/review-fixes.md) | 19 项问题的修复、回归验证、接口兼容变化与升级顺序 |
| [源码来源](docs/provenance.md) | 上游修订、导入清单、内容摘要与后续合并说明 |

## 模块布局

```text
api/v1alpha1/             PulsarCluster、BookKeeperCluster API
cmd/operator/            Go Operator 入口
internal/                controller、resources、console、metrics
web/                     Pulsar React 管理页面
config/                  Pulsar CRD、RBAC、部署与样例
modules/storage/         独立可运行的磁盘管理模块
  diskfleet/             节点 Agent、管理 API、前端、LVM 安全执行
  deploy/                PhysicalDisk / DiskPool CRD、Agent 与 API
  integrations/pulsar/   存储等级生成器的契约与离线样例
  tests/ scripts/ docs/   模块测试、验收、磁盘手册
deploy/all/              整体 Kustomize 入口
deploy/two-node/         172.23.254.149 / .150 两节点调度 overlay 与 CSI values
deploy/console/          统一入口网关：/ 与 /storage/
scripts/                 仓库级工具入口
test/e2e/                Pulsar、存储、联合验收入口
tests/                   仓库布局、部署与入口回归测试
docs/                    统一架构、设计、指南与验证报告
```

## 快速开始

仅需 Python 3.10+ 即可体验磁盘模拟管理页面：

```bash
cd pulsar-operator
make demo-storage
# 打开 http://127.0.0.1:8080
```

模拟模式提供 24 台节点、144 块磁盘的数据与管理流程，不初始化物理盘，不运行 Pulsar。真实 Pulsar 页面与统一网关需要部署 Kubernetes 版本，见[使用指南](docs/user-guide.md)。

```bash
make test-offline          # Python 模块与仓库检查；安装 kubectl 时也验证部署渲染
make test                  # 另外运行 Go/envtest、Node 单测及 Vite 构建
make images               # 构建 pulsar-operator:dev、pulsar-operator-storage:dev
make render               # 渲染整体部署；不连接集群、不执行 apply
make preflight-two-node   # 只读检查已加入集群的 .149/.150，默认 /etc/kubernetes/admin.conf
```

统一部署后通过 `kubectl -n pulsar-operator-system port-forward svc/console-gateway 8080:8080` 打开控制台。左侧为运行总览、Broker 组件、Bookie 组件和物理存储；存储的七个页面直接显示在同一内容区。Broker 与 Bookie 独立管理，关联表与详情提供双向跳转。两套 API 使用各自管理 Token，网关不提供单点登录；`/storage/` 仍可独立访问。

## 当前能力与适用范围

- 支持节点发现、物理盘身份核验、批量接管计划、每盘独立 VG、StorageClass、PVC 可视化与审计。
- 提供跨节点 fleet StorageClass，把 journal 与 ledger 放到独立物理池，检查多副本拓扑交集与容量。
- 默认关闭磁盘写入；使用 by-id、序列号、精确容量、文件锁和持久化日志防止误纳管、重复执行。
- 保留 Pulsar / BookKeeper 控制器与监控功能，提供消息读写、Pod 重建、卷扩容、Retain 的联合验收脚本。

本次已在 `172.23.254.149/.150` 上使用两块 100 GiB 数据盘完成真实 Kubernetes/LVM/CSI/Pulsar 联调：9 个业务 PVC、20 条消息跨 Bookie 重建保留、3 个 ledger 卷在线扩到 21 GiB、独立 Retain 卷保留均通过。统一页面入口为 `http://172.23.254.149:30080`（同私网也可通过 `.150:30080`），登录命令见[数据盘部署指南](docs/shared-data-disk-lab.md)。

这套两节点共享数据盘配置用于实验验证，journal/ledger 共用物理盘，不保证主机故障可用，也未证明万台物理机规模。存储 API 当前单副本、全量快照缓存、SQLite 审计，部署默认 `emptyDir`；生产需持久化并按规模演进。OpenEBS LVM CSI 是外部依赖，本地 PV 不提供跨节点复制。具体结果与限制见[验证报告](docs/validation.md)。
