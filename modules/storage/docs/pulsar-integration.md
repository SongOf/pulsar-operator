# 同仓库 Pulsar Operator 与物理存储集成

Pulsar Operator 基于用户指定的 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator) `3c042c5`，已与 DiskFleet 一起纳入当前仓库：三个后端共享根 Go 项目，分别由模块脚本管理。应构建当前整合版，不能另行 checkout 上游旧修订覆盖集成改动。导入来源见[源码清单](../../../docs/provenance/upstream-source.json)，已核实的资源契约见 [source-contract.json](../../../docs/provenance/pulsar-storage-contract.json)。历史两节点真实 Kubernetes/CSI/Pulsar 记录仅适用于当时的镜像，目标版本需另行验收，范围见[统一验证报告](../../../docs/history/validation.md)。

本页所有命令默认从统一仓库根目录 `pulsar-operator/` 执行。模块脚本自动定位仓库；规划参数中的相对文件路径以调用者目录为准。

本文保留默认 `three-node-e2e` 的三节点验收流程。已有 `172.23.254.149 / .150` 两节点请使用[两节点指南](../../../docs/operations/two-node.md)。当前每节点一块数据盘使用显式 `--profile two-node-shared-disk-lab`，见[共享数据盘指南](../../../docs/labs/shared-data-disk.md)；每节点 journal/ledger 分盘才使用 `two-node-lab`。两者均不满足三节点故障域隔离，不能直接运行原三节点 E2E。

## 集成约束

| 问题 | 修复 |
|---|---|
| 单盘类仅允许一个 hostname，无法为三个硬反亲和副本提供三个节点 | 为 ZooKeeper、journal、ledger 生成三个跨节点 StorageClass，继续使用 WFFC 和真实 hostname |
| 同一 Bookie 的 journal/ledger 可能没有共同可用节点 | 生成前要求三个存储等级覆盖相同节点；每等级每节点一个池，至少三节点 |
| journal/ledger 可能被错误配置到同一物理池 | 强制两组池不重叠，并拒绝跨节点 VG 同名造成的角色混选 |
| 没有 DiskFleet managed 标签的 Pulsar PVC 不可见 | 按已验证的 StorageClass/池 UID 观察外部 PVC，不改其标签和所有权 |
| 两个 Operator 都能修改同一 PVC，期望状态相互覆盖 | 外部 PVC 标记只读，API 拒绝直接扩容/释放；即使误加 managed 标签，也识别外部 managed-by 标签或 ownerReferences |
| Retain 被误解为缩容后一定不删除 PVC | 区分 Pulsar 对 PVC 的策略与 CSI 对 PV 的策略；Bookie decommission 后 PVC 可删除，而 PV/LV 仍 Retain |

Pulsar 源码实际使用 `size` 和 `storageClassName` 字段，BookKeeper 每副本生成 journal/ledgers 两个 RWO PVC。Pod 已配置 `fsGroup=0` 与 `OnRootMismatch`。[类型契约](https://github.com/SongOf/pulsar-operator/blob/3c042c5/api/v1alpha1/common_types.go)、[资源生成](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/resources/bookkeeper.go)、[Pod 配置](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/resources/common.go)。

## 测试布局

默认至少三台节点，每节点两块已纳管的独立盘：

| 每节点存储池 | 业务用途 | 最低可用容量 |
|---|---|---|
| journal 池 | 5 GiB journal + 5 GiB ZooKeeper（此测试 profile 共享） | 10 GiB |
| ledger 池 | 20 GiB ledger + 1 GiB 扩容余量 + 1 GiB 保留探针余量 | 22 GiB |

也可给 ZooKeeper 独立的第三块盘。容量检查会累计同池的多个角色。默认创建 3 ZooKeeper、3 Bookie、2 Broker、1 Proxy；每个 Bookie/Broker 的 memory request 为 3 GiB，应先核对集群可调度资源。监控在这个存储验收 profile 中关闭。

`pools.example.json` 与 `manifest.example.json` 是**虚构池 UID 的离线示例**，用于审查和回归测试，不能直接应用到真实集群。真实清单必须由当前 Ready DiskPool 信息生成。

## 1. 构建并部署当前整合版

在可联网、具备容器构建能力的环境，进入已有的整合仓库并为本次构建选择不可变标签：

```bash
cd pulsar-operator
make images \
  IMG=YOUR_REGISTRY/pulsar-operator:integrated-v1 \
  STORAGE_IMG=YOUR_REGISTRY/pulsar-operator-storage:integrated-v1 \
  AGENT_IMG=YOUR_REGISTRY/pulsar-operator-agent:integrated-v1
```

将 Pulsar、Storage API、Agent 三份镜像推送到自己的 registry 或分发到测试节点。构建参数不会改写部署文件：在 `config/pulsar/manager/manager.yaml` 将 `operator` image 替换为 Pulsar 镜像或其 digest，在 `config/storage/manager/api.yaml` 使用存储 API 镜像，在 `config/agent/manager/agent.yaml` 使用独立 Go Agent 镜像。先按[存储模块部署指南](usage.md#真实集群部署)建立所需 namespace/Secret、安装 CSI，并按[仓库部署说明](../../../docs/operations/deployment.md)设置 Pulsar 控制台凭证，再应用统一清单：

```bash
kubectl apply -k deploy/bundles/all
kubectl -n pulsar-operator-system rollout status deployment/pulsar-operator
kubectl -n diskfleet-system rollout status deployment/diskfleet-api
kubectl get crd pulsarclusters.pulsar.songof.dev bookkeeperclusters.pulsar.songof.dev
```

`3c042c5` 只表示导入基线，集成版镜像还包含当前仓库改动，应记录当前源码版本及镜像 digest。根 [Makefile](../../../Makefile) 与 [deploy/bundles/all](../../../deploy/bundles/all/kustomization.yaml) 是实际构建和组合入口；生产部署还需固定 Gateway 镜像版本或 digest。

## 2. 生成并审核 DiskFleet 存储对接清单

先按 Storage 使用说明纳管测试盘，确认 OpenEBS LVM CSI 可用。准备脚本只读集群，不执行初始化或 kubectl apply：

```bash
kubectl get diskpools -o wide

# 将下列名称替换为实际池名；三个列表中的节点范围必须相同。
modules/storage/scripts/dev.sh plan \
  --zookeeper-pools journal-node-a,journal-node-b,journal-node-c \
  --journal-pools journal-node-a,journal-node-b,journal-node-c \
  --ledger-pools ledger-node-a,ledger-node-b,ledger-node-c \
  --namespace diskfleet-pulsar-e2e --name diskfleet-pulsar \
  --output /tmp/diskfleet-pulsar-plan.json
```

脚本复用 DiskFleet 的 Kubernetes 连接：Pod 内自动使用 ServiceAccount；本地可先运行 `kubectl proxy --address=127.0.0.1 --port=8001`，然后为脚本设置 `KUBE_API_URL=http://127.0.0.1:8001`。它不会自动读取 kubeconfig 或关闭 TLS 校验。

审核生成的 namespace、三个 StorageClass 的精确 VG 白名单/hostname/池 UID 和 PulsarCluster 后执行：

```bash
# Namespace 必须先真实存在，后面的服务端 dry-run 才能验证其中的 CR。
kubectl create namespace diskfleet-pulsar-e2e --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --dry-run=server -f /tmp/diskfleet-pulsar-plan.json
kubectl apply -f /tmp/diskfleet-pulsar-plan.json
kubectl -n diskfleet-pulsar-e2e get psc,bkc,pod,pvc -w
```

StorageClass 参数不可随意修改；加入新节点/新池时应生成新类，并遵循 Pulsar Operator 的数据迁移流程。不能直接把已存在 PVC 的 StorageClass 改成新类。

## 3. 执行真实验收

等待 PulsarCluster Running 且 observedGeneration 已追上 generation，再运行：

```bash
TIMEOUT=600 bash test/e2e/system/pulsar-storage.sh diskfleet-pulsar-e2e diskfleet-pulsar
```

完整步骤、保留的资源和未覆盖场景见 [Pulsar E2E 手册](../../../docs/testing/pulsar-storage-e2e.md)。该脚本会重建 bookie-0、扩大 ledger 卷、验证消息与文件数据，并用独立探针验证 Released/Retain；不会直接删除 Pulsar 业务 PVC，也不会初始化磁盘。

Pulsar 扩容必须修改父 `PulsarCluster.spec.bookkeeper.storage.ledger.size`，由该 Operator 更新 PVC 和 StatefulSet 模板。本项目的前端只观察这些卷。[扩容与缩容控制器](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/controller/bookkeeper_controller.go)。

## 本机可复现检查

```bash
make test-storage test-repository

# 离线生成：只验证配置与输入数据，不代表已连接或创建集群。
modules/storage/scripts/dev.sh plan \
  --pools-json test/fixtures/pulsar-storage/pools.example.json \
  --zookeeper-pools journal-0,journal-1,journal-2 \
  --journal-pools journal-0,journal-1,journal-2 \
  --ledger-pools ledger-0,ledger-1,ledger-2 \
  --namespace diskfleet-pulsar-e2e --name diskfleet-pulsar \
  --output /tmp/diskfleet-pulsar-offline-plan.json
```

本地 Storage Go 测试覆盖契约、原 Python 规划清单兼容、SQLite 及 fake Kubernetes API；控制器 envtest 使用独立 API Server/etcd，不含调度器和 CSI。当前另有两节点真实运行记录，包括消息收发、Bookie 重建、卷扩容和 Retain；具体镜像及证据见[统一验证报告](../../../docs/history/validation.md)，不据此推定生产规模或节点失效恢复已经验证。
