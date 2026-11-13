# 使用 SongOf/pulsar-operator 验证 DiskFleet

本文未另行标注的命令从统一仓库的 `pulsar-operator/` 仓库根目录执行。Pulsar 与存储模块均已在同仓库；仓库根的默认三节点入口为 `test/e2e/system/pulsar-storage.sh`，两节点共享数据盘入口为 `test/e2e/system/pulsar-shared-disk.sh`。

`test/e2e/system/pulsar-storage.sh` 针对用户指定的 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator) 编写。资源与字段依据已检查的 `3c042c5` 版本源码，而不是 Apache Helm chart 或另一个同名 Operator。

**2026-11-13（北京时间），共享盘入口已在 172.23.254.149/150 的真实集群完整通过。** 测试对象为 `diskfleet-pulsar-data-lab/pulsar-lab`，9 个业务 PVC 使用真实 LVM CSI，全部 ledger 已从 20Gi 扩至 21Gi。记录见下文“本次实际结果”。原三节点入口的拓扑和分盘限制保持不变，本次没有宣称通过三节点验收。

## 入口与保护条件

| 入口 / profile | 标签与拓扑 | 角色池 |
|---|---|---|
| 默认 `three-node-e2e` / 根 `test/e2e/system/pulsar-storage.sh` | Namespace 与 PSC 的 `integration=pulsar`；至少三副本、Required，实际副本分散到不同节点 | journal/ledger 必须为不同物理 DiskPool；拓扑交集满足全部副本 |
| 显式 `two-node-shared-disk-lab` / 根 `test/e2e/system/pulsar-shared-disk.sh` | Namespace、PSC、SC 的独立 integration/profile 标签；恰好两个已标记 Ready 可调度节点；3 ZK、3 Bookie、2 Broker、1 Proxy、1 AutoRecovery，Preferred、minReplicas=3、quorum=2/2/2 | 三角色必须选择同一组恰好两个 Ready 池，每个节点一个池，精确 UID/VG/hostname 核对 |

上述标签的完整前缀为 `storage.diskfleet.io/`；共享 integration 值是 `pulsar-two-node-shared-disk-lab`，profile 值是 `two-node-shared-disk-lab`，节点选择器为 `pulsar.songof.dev/two-node=true`。共享模式允许实际副本共置，不强称 Preferred 能保证 2+1。所有组件和独立 Retain 探针有 control-plane/master NoSchedule 容忍。`two-node-lab` 不属于这两个 E2E 入口接受的 profile，不通过伪改测试标签绕过检查。

## 验收对象

该 Operator 的 `PulsarCluster` API 为 `pulsar.songof.dev/v1alpha1`。ZooKeeper 数据卷、BookKeeper journal/ledger 分别使用 `spec.zookeeper.storage`、`spec.bookkeeper.storage.journal`、`spec.bookkeeper.storage.ledger` 下的 `size` 和 `storageClassName`。BookKeeper 每个副本有两个 RWO PVC。来源：[类型定义](https://github.com/SongOf/pulsar-operator/blob/3c042c5/api/v1alpha1/bookkeepercluster_types.go)、[资源生成器](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/resources/bookkeeper.go)。

默认三节点 profile 的三个 BookKeeper 副本需要可以在至少三台机器上同时提供 journal 与 ledger 的存储类。单盘单节点 StorageClass 无法满足 `pod.antiAffinity: Required` 的多节点放置；两个角色的节点范围还必须有足够大的交集。DiskFleet 的默认集成配置使用跨节点 fleet StorageClass，journal 与 ledger 对应不同的物理 DiskPool，ZooKeeper 也使用三节点存储类。共享盘实验 profile 的独立规则见上表，不改变这些默认前提。

## 默认三节点验收的准备

1. 准备专用测试节点与空白测试盘，部署 DiskFleet 和 OpenEBS LVM CSI，确保参与测试的 DiskPool 均为 Ready。journal 与 ledger 应分盘，每个 ledger 池除业务初始申请量外至少预留 2 GiB 用于本脚本的扩容和独立保留探针。
2. 按项目集成配置生成器建立三类 fleet StorageClass，并创建专用 namespace 与 PulsarCluster；namespace 名称必须以 `diskfleet-pulsar-` 开头，二者都必须有标签 `storage.diskfleet.io/integration: pulsar`。脚本拒绝不符合这些条件的资源。
3. 从固定版本构建并部署指定 Pulsar Operator，依据仓库自己的部署说明安装 CRD/RBAC/控制器。不将浮动分支的新行为视为本次验收已经覆盖。
4. 测试 profile 至少使用三个 ZooKeeper、三个 BookKeeper，并为二者设置 `pod.antiAffinity: Required`。BookKeeper `storage.reclaimPolicy: Retain`；ledger 容量用整数 `Gi`。监控可关闭以控制测试资源量。
5. 等待 PulsarCluster `status.phase=Running` 且 `status.observedGeneration` 等于当前 generation。集群必须可以拉取 Pulsar 与探针容器镜像。脚本需要 `kubectl`、Python 3、GNU `timeout`、`mktemp` 与 `awk`。

```bash
# 此命令会在专用测试集群中重建 bookie-0 并扩大卷容量。
TIMEOUT=600 bash test/e2e/system/pulsar-storage.sh <测试namespace> <PulsarCluster名称>
```

`TIMEOUT` 是每步的超时，允许 60–1800 秒；总耗时会更长。`TEST_IMAGE` 默认为 `busybox:1.37.0`，离线环境可替换成内网同等镜像。脚本不自行安装 Operator、不初始化磁盘、不创建 VG。

## 两节点共享数据盘验收

先完成[共享数据盘配置](../labs/shared-data-disk.md)，确保真实池和业务对象使用上表的独立标签，Running/observedGeneration 已收敛。初次部署每个池按 94Gi 可用容量检查最坏共置情况，包括全部业务、三 ledger 各 1Gi 扩容和 1Gi Retain 探针。现有集群已占用容量，重复验收需核算实时剩余空间。

从**仓库根**执行：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
# 只读预检：不执行文件写入、消息、Pod 重建、扩容或 Retain 生命周期。
PREFLIGHT_ONLY=1 TIMEOUT=600 bash test/e2e/system/pulsar-shared-disk.sh \
  diskfleet-pulsar-data-lab pulsar-lab

# 完整验收：会重建 bookie-0、再扩每个 ledger 1Gi，留下一个 1Gi Retain 探针。
TIMEOUT=600 bash test/e2e/system/pulsar-shared-disk.sh \
  diskfleet-pulsar-data-lab pulsar-lab
```

`test/e2e/system/pulsar-storage.sh` 始终使用默认三节点约束；`test/e2e/system/pulsar-shared-disk.sh` 显式选择共享盘 profile，两者复用 `test/e2e/lib/pulsar-storage.sh`。共享入口不能用于宣称节点故障高可用或 journal/ledger 物理隔离。

## 实际检查顺序

1. 按明确 profile 校验 namespace/CR 的测试标签、真实 API group、Running 状态和声明的存储类。检查 CSI 驱动、厚置备、WFFC、Retain、可扩容、池 UID/VG 白名单和节点拓扑。共享模式额外验证实时两个 Ready 节点与两池身份。
2. 等待目标 Operator 创建的 ZooKeeper 数据、BookKeeper journal/ledger PVC 绑定，校验实际 PV 的 CSI driver、claim UID、Retain、节点亲和性和 Pod 卷引用。三副本检查九个业务 PVC；默认模式要求各组件副本分布到不同节点，共享模式要求实际 Pod 只使用两个核实节点。只读模式在完成这些检查、保存探针计划后退出，明确输出尚未执行后续生命周期验收。
3. 使用镜像实际用户在 bookie-0 的 journal/ledger 两个挂载目录内写入唯一隐藏标记文件并同步，启用 noclobber 防止覆盖已有文件，记录实际 `id`。在真实 broker 中创建唯一订阅并生产 20 条带测试标记的消息。重建 bookie-0，确认 Pod UID 改变且重新 Ready，检查两个原文件仍能由镜像实际用户读取，再消费原有消息。文件检查用于验证该 bookie 的卷原地保留；消息检查同时覆盖 Pulsar 服务数据可用性，不能单独替代本地文件检查。
4. 通过 **PulsarCluster spec** 将 ledger 容量增加 1 GiB，不直接修改 Pulsar Operator 管理的 PVC。等待全部 ledger PVC 实际容量更新、检查 bookie-0 文件系统在线增长，并确认所有原有 PVC/PV UID 与绑定保持不变。该 Operator 的扩容与 StatefulSet 模板维护路径见 [控制器实现](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/controller/bookkeeper_controller.go) 与 [资源应用逻辑](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/controller/apply.go)。
5. 在同一 ledger 存储类上创建独立的 1 GiB 探针 PVC/Pod，写入标记后删除本次创建的探针 Pod/PVC，验证原 PV 进入 Released 且维持 Retain。不会删除任何 Pulsar 业务 PVC。

消息发送/消费命令与输出判定参考指定仓库的 [原始 E2E 脚本](https://github.com/SongOf/pulsar-operator/blob/3c042c5/test/e2e/e2e.sh)。本脚本不是原始脚本的完整替代，不执行其 bookie 扩缩容/decommission 测试；这些需要额外节点、容量与更长验收窗口。

## 结果与保留内容

完整模式只有全部实际检查成功才输出最终 `PASS [profile]`。只读模式的 `PASS: read-only preflight` 明确注明未执行持久化、扩容及 Retain，不能据此声称完整通过。任何 Kubernetes、CLI、数据、容量或拓扑检查失败都会退出非零；超时不会被当成成功。脚本保留本次证据目录，包括 CR、PVC/PV UID、节点拓扑、消息 CLI 日志、探针清单和扩容前后文件系统容量。失败时还会收集 namespace 状态与事件。

成功后仍会保留：

- 运行中的测试 PulsarCluster，ledger 容量比开始时大 1 GiB。不能通过回写旧值“回滚”扩容。
- 一块 Released 的 1 GiB 探针 PV/LV，用于确认 Retain；保留的空间不会自动回收。
- 本次唯一测试 topic/subscription 与本地证据目录。
- bookie-0 journal 和 ledger 挂载目录各一个 `.diskfleet-e2e-<run-id>` 隐藏标记文件；脚本输出准确文件名，不批量删除文件。

根据输出记录人工审查清理，不运行 `lvremove`、`vgremove`、`wipefs`，不删除宿主磁盘日志。测试 namespace 不能误用生产 namespace，即使人为给生产资源加上测试标签也不会使这类操作适合生产。

历史执行记录见[联合验收记录](../history/pulsar-storage-e2e.md)，不代表当前 Go 版本已完成目标环境验收。
