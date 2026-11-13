# 整合版本验证记录

> 本文是特定镜像的历史部署与验证结果，保留当时的语言、测试计数和环境。它不代表当前工作区或后续版本已经完成真实集群验收。

日期：2026-11-13（Asia/Shanghai）。源码位于 `/root/workspace/pulsar-operator`，已核实并恢复上游 `3c042c57a79e83cd3f329c7088a65e2bc65a62af` 的 Git 历史。原始部署证据位于 `/root/workspace/pulsar-deployment-evidence`；本次 19 项审查修复、隔离回归和升级证据位于 `/root/workspace/pulsar-review-fixes-20261113`。机器可读摘要为 [verification.json](verification.json)，逐项说明见[审查修复记录](review-fixes.md)。

## 实际部署

| 项目 | 实际结果 |
|---|---|
| Kubernetes | 既有 v1.37.2 集群；`.149` 控制平面与 `.150` 工作节点均 Ready；保留控制平面 taint |
| 网络 | Flannel v0.28.10，2 个节点 Pod Ready；CoreDNS 2/2；跨节点 Service/DNS HTTP 探测通过 |
| 管理面 | Pulsar Operator、统一 nginx 网关、DiskFleet API Ready；DiskFleet Agent 每节点 1 个 |
| CSI | OpenEBS LVM LocalPV chart/driver 1.10.1；控制器及两节点 CSI Ready，CSINode 注册通过 |
| 数据盘 | 每节点 `/dev/vdb` 100 GiB；按用户批准的 by-id、serial、精确容量由 Agent 初始化；两系统盘未进入 LVM |
| 存储池 | `lab-data-149` / `lab-data-150`，VG `df_lab_data_149` / `df_lab_data_150`；均 Ready；PV UUID、所有权标签和持久化日志匹配 |
| Pulsar | `diskfleet-pulsar-data-lab/pulsar-lab`；PulsarCluster、对应 BookKeeperCluster 均 Running |
| 业务组件 | ZooKeeper 3、Bookie 3、Broker 3、Proxy 1、AutoRecovery 1；11 个业务 Pod Ready，初始化 Job Completed。早先存储 E2E 使用 2 个 Broker，升级前已改为 3，本次保留当前 spec |
| 业务存储 | 9 个 CSI PVC Bound；ZooKeeper 3×5 GiB、journal 3×5 GiB、ledger 3×21 GiB |
| 内置监控 | 升级前已启用，但 Prometheus 的另一个 PVC 未指定可用存储类，仍为 Pending；该已有配置与卷身份保留，不能据 Pulsar Running 声称监控已就绪 |
| 入口 | `http://172.23.254.149:30080` 与 `http://172.23.254.150:30080`；同源 `/storage/` 与 Pulsar 页面共用网关 |

实际使用独立的 `two-node-shared-disk-lab` profile：3 个存储角色使用相同的两个数据池，Preferred 反亲和，quorum ensemble/write/ack=2/2/2。没有降低默认三节点验收的角色物理隔离要求；本次通过不能记作三节点生产拓扑验收通过。详细重现步骤、两套登录 Token 的获取命令及升级方法见[共享数据盘指南](../labs/shared-data-disk.md)。

## 构建与回归

| 检查 | 结果 | 实际执行方式 |
|---|---|---|
| 存储 Python | 150 项通过 | 身份独占、跨进程锁、旧日志迁移、PV 实际归属、释放响应丢失/同名重建恢复 |
| 仓库 Python | 48 项通过 | 包含 19 项部署保护、11 项共享盘预检及 6 项新增脚本回归；最终源码摘要检查单独复核通过 |
| Go vet / Go 测试 | 73 个顶层测试通过 | Controller 36（新增 18）、Console 19、Resources 14、Metrics 4；实际 envtest API server / etcd 1.36.2，未跳过 |
| Web 模型 | 34 项通过 | 实际 Node.js `npm test`，包括编辑基线、原子写入前提和 URL Token 退役 |
| React/Vite | 构建通过 | 实际 `npm ci` / `npm run build`；构建产物已随 Operator 镜像部署 |
| 存储 UI JS | 41 项通过 | 实际 Node.js 执行 `check-ui.cjs`，含绑定失败/历史消息转义、冲突池及确认时 PVC UID |
| 镜像 | 两模块构建、两节点导入通过 | 官方 BuildKit v0.35.0 使用现有 containerd；未安装 Docker 或替换 Kubernetes runtime |
| Kustomize / 准入 | 通过 | 聚合与两节点 overlay 各 23 个资源；真实 API server 对生成的 Pulsar 计划 dry-run 并接受部署 |

envtest 版本与实际集群版本不同：1.36.x 用于控制器回归，1.37.2 上的部署与存储链路由下列运行证据覆盖。版本兼容结论仅限本次组合与操作范围。

运行时修复包括：Agent 只读挂载宿主 `/run/udev`，以取得真实磁盘序列号；发现算法排除 `lvm-pv-uuid-*` 元数据别名，防止初始化后替换已批准的物理盘身份。签名、挂载、holder、序列号、容量与 WWN 优先检查仍保留。部署脚本也覆盖了空 CSINode drivers 和 kubectl 多文档 JSON 输出，避免正常现场被错误拒绝。

构建证据：`build-verification.json`、`verify-build.log`、`operator-build.json`、`storage-build-v2.json`。Python 输出：`storage-alias-regression-tests.log`、`repository-final-tests.log`。镜像 digest 与最终运行状态记录在机器可读摘要和现场证据中。

上述文件名是首次部署记录。本次修复采用独立证据目录中的 `controller-proof/`、`console-proof/`、`storage-proof/`、`repository-tests.log`，并构建和部署 `localhost/pulsar-operator:review-20261113-v3` 与 `localhost/pulsar-operator-storage:review-20261113-v3`。两个镜像在两节点的 digest 均已核对；先应用新增恢复字段的 CRD，再更新 Operator 和前端，网关及存储组件分别滚动升级。

升级后核验 31 个实际 HTTP 请求：两节点页面/静态资源/独立鉴权、业务卷实际绑定、缺少或错误写入前提的拒绝响应，以及假 Token 不进入 nginx 日志。9 个业务 PVC/PV、11 个运行中业务 Pod 的 UID 均未改变，ledger 保持 21 GiB，父 Pulsar spec 与升级前完全一致。两块盘的 PV UUID、VG tag 和设备身份不变，宿主日志仅补记 poolUID；系统盘仍未进入 LVM。API 原有 1 条 Retain 历史和 2 条审计经一致性备份后合并恢复。证据为 `runtime-verification.json`、`after-lvm-inventory.json`、`api-state-restore.json`。

升级前发送的 20 条保留测试消息，在 Operator 升级后全部消费成功，见 `messages-verification.json` 及 produce/consume 日志。此验证没有重建业务 Pod、再次扩卷或创建新 PV；历史的完整 Bookie 重建/扩容 E2E 记录仍单独保留。

## 真实 Pulsar / 数据盘 E2E

实际执行入口：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
make e2e-pulsar-shared-disk NAMESPACE=diskfleet-pulsar-data-lab CLUSTER=pulsar-lab
```

本次已执行成功；上述命令再次执行会再扩大 ledger 卷并创建新的保留测试资源，不应仅为查看状态重复运行。只读预检可用：

```bash
PREFLIGHT_ONLY=1 bash test/e2e/pulsar-shared-disk.sh diskfleet-pulsar-data-lab pulsar-lab
```

本次证据为 `shared-e2e.log` 和 `diskfleet-pulsar-e2e.WnikXAer/`，各步骤均实际运行：

1. 核对 2 个 Ready 节点、真实池 UID、VG、3 个 StorageClass、9 个 PVC/PV 的 CSI、节点拓扑与挂载。
2. 用 Bookie 镜像用户（UID 10000）在 journal 和 ledger 写入唯一标记，并向实际 Broker 发送 20 条保留消息。
3. 重建 Bookie-0，确认新 Pod、原 PVC/PV 身份、标记文件均保留；消费全部 20 条消息成功。
4. 通过父 PulsarCluster 将 3 个 ledger 从 20 GiB 扩到 21 GiB；9 个 PVC/PV UID 和绑定保持；各 ledger 的实际文件系统增大，Bookie-0 `df` 总量从 20466256 KiB 增至 21498384 KiB。
5. 单独创建 1 GiB 测试卷，写入标记并删除测试 Pod/PVC；原 PV `pvc-0d567184-691a-4d14-9408-9237367358b7` 进入 Released，Retain 保留。未删除业务 PVC。

测试 topic/subscription `diskfleet-133d4af9d5f1`、Bookie 文件 `.diskfleet-e2e-133d4af9d5f1` 与独立 1 GiB Released PV/LV 按设计保留，不自动擦除。Pulsar 继续以 ledger 21 GiB 运行；没有缩容回滚。

系统盘目录替代方案此前只进行过两个测试卷的重建/文件保留与跨节点网络探测；其测试资源保留，系统盘版 Pulsar 清单从未部署。本次业务 9 个卷全部使用两块数据盘的 LVM CSI。详见[系统盘实验说明](../labs/system-disk.md)。

## 管理页面与验证边界

两台机器的 NodePort 上已实际请求首页、JS/CSS、嵌入存储页、API 配置与健康接口。验证未认证和错误模块 Token 返回 401，各自正确 Token 返回 200；真实 Pulsar/BookKeeper 严格 UID 关联、2 个 Ready 池、9 个外部业务 PVC 的只读边界均通过。最终 HTTP 与磁盘清单核验见 `console-http-validation.json`。没有在日志或报告中记录 Token 内容。

本次未完成真实浏览器 DOM、窄屏布局、键盘焦点或点击交互验收；HTTP、构建与模型测试不能替代这些检查。也未验证节点/磁盘掉电、Bookie decommission、跨集群故障迁移、主机重启后的恢复或生产规模性能。两节点共享 journal/ledger 不提供物理 I/O 隔离，不能据此声明主机故障 HA。

API 仍为单副本、快照缓存和 SQLite 审计，默认 `emptyDir` 不保证审计跨 Pod 重建保留；本次 Agent 镜像更新不会丢失宿主机 `/var/lib/diskfleet` 的磁盘所有权日志，但 API 审计与该日志不是同一份数据。大规模物理盘纳管的演进方案见[架构设计](../design/architecture.md)，尚未做万机压测。
