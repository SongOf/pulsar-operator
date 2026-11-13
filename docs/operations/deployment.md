# 使用指南

本文命令除特别说明外均从 `pulsar-operator` 仓库根目录执行。已有 Pulsar 项目代码已包含在仓库中，无需另克隆一份。完整架构见 [architecture.md](../design/architecture.md)，模块开发见 [project-design.md](../design/repository.md)。

## 1. 选择运行方式

| 模式 | 所需环境 | 能验证什么 |
|---|---|---|
| 离线测试 | Go（版本见 go.mod）与仓库检查所用 Python；安装 kubectl 后额外运行部署渲染检查 | 身份保护、恢复、分配逻辑、API 契约、配置组合 |
| 本地存储演示 | Go、允许监听回环端口；构建后的二进制不需要 Python | 模拟盘/池/PVC 的管理流程与页面 |
| 控制器开发 | Go（版本见 go.mod）、Node 22、npm、envtest 二进制 | Go 控制器测试、React 构建；envtest 不含 CSI/kubelet |
| 真实联合验收 | Linux Kubernetes、镜像构建/分发、空白测试盘、OpenEBS LVM CSI | VG→LV→PVC→Pod、Pulsar 消息、重建、扩容、Retain |

本地模拟卷是目录，容量由元数据控制，不是块设备或文件系统配额。真实集群模式不会把 Kubernetes 请求悄悄降级成模拟成功。

```bash
scripts/dev.sh init
make test-offline
make demo-storage
# 浏览器打开 http://127.0.0.1:8080；Ctrl+C 停止
```

模拟状态保存在 `modules/storage/.state/state.sqlite3`。需要换端口时使用 `API_PORT=8082 make demo-storage`。历史两节点 Kubernetes、CSI 和 Pulsar 联合验收记录见[验证报告](../history/validation.md)，它仅覆盖记录中的镜像，不能替代目标版本的真实集群验收。

## 2. 构建与镜像

```bash
scripts/dev.sh init --integration
make test
make images IMG=YOUR_REGISTRY/pulsar-operator:integration-v1 \
  STORAGE_IMG=YOUR_REGISTRY/pulsar-operator-storage:integration-v1 \
  AGENT_IMG=YOUR_REGISTRY/pulsar-operator-agent:integration-v1
docker push YOUR_REGISTRY/pulsar-operator:integration-v1
docker push YOUR_REGISTRY/pulsar-operator-storage:integration-v1
docker push YOUR_REGISTRY/pulsar-operator-agent:integration-v1
```

把 `YOUR_REGISTRY` 替换为可用的私有镜像仓库。Pulsar 镜像包含 Go Operator 与构建后的 React 页面；storage 镜像包含 Go API 和嵌入的存储页面；agent 镜像运行 Go 节点 Agent 及 Linux LVM 工具。三个 Dockerfile 均位于 `modules/<模块>/Dockerfile`，从仓库根 context 构建；可用 `make image-pulsar`、`make image-storage`、`make image-agent` 分别构建，原 `make image` 仍构建 Pulsar。三个后端运行镜像不需要 Python。网关使用 nginx 镜像。离线节点应预先同步所有镜像，包括 Pulsar、监控和 E2E 的 busybox；正式发布固定镜像 digest。

默认整体部署使用本地开发镜像 `pulsar-operator:dev`、`pulsar-operator-storage:dev` 与 `pulsar-operator-agent:dev`。部署自有镜像时修改 `deploy/bundles/all/kustomization.yaml` 的 `images`，或创建引用该目录的自有 overlay；仅构建/推送镜像不会自动修改部署配置。

Go Agent 可单独运行 `make test-agent` 和 `make build-agent`；Storage 使用 `make test-storage` 和 `make build-storage`，构建产物为 `bin/diskfleet-api`、`bin/storage-plan`、`bin/storage-benchmark`。这些单元测试不访问真实设备。仅安装 Storage API 与 Agent 时使用 `deploy/bundles/storage`。

升级前保留 Agent 宿主日志，并完成 Storage API `STATE_DIR` 的一致性备份。管理状态由独立 PVC 保存；SQLite 使用 WAL 时，备份必须包含已提交到 WAL 的数据。现有临时目录中的状态须在替换 Pod 前完成一致性备份与恢复。

## 3. Kubernetes 与 CSI 准备

使用专用测试集群。Pulsar 存储验收默认至少三台工作节点，每台至少两块独立空白盘：journal 池可与 ZooKeeper 共用，ledger 必须独立。每台 journal 池至少预留 10 GiB，ledger 至少 22 GiB；这是默认测试 profile 的下限，并非生产容量建议。每个 Bookie/Broker 默认 memory request 3 GiB，还要为 ZooKeeper、CSI 和系统组件留足资源。

节点准备 LVM2、udev、util-linux、device-mapper、稳定 `/dev/disk/by-id`，允许所选节点运行特权 Agent。真实物理盘需要能够透传到 Linux；普通容器版 kind 不自动具备真实主机盘的接入能力。

先独立安装 OpenEBS LVM LocalPV CSI；本仓库的 `deploy/bundles/all` 不安装 CSI。使用经过目标集群验证的固定 chart 版本，安装和参数以 [OpenEBS LVM LocalPV 官方仓库](https://github.com/openebs/lvm-localpv) 为准。已有驱动不重复安装。默认节点 kubelet 根路径应与 CSI 配置一致。

```bash
kubectl config current-context
kubectl get nodes -o wide
kubectl get csidriver local.csi.openebs.io
kubectl -n openebs get pods -o wide
```

看到 CSIDriver 对象不等于能正常创建和挂载卷；最终以真实 PVC/Pod 验收为准。

## 4. 部署统一项目

先准备两个独立管理 PVC：`diskfleet-system/diskfleet-api-state` 与 `pulsar-operator-system/pulsar-operator-audit`，默认各 1Gi。清单不指定 StorageClass，使用集群默认类；也可通过 overlay 选择已就绪的基础设施类或绑定静态 PV。没有可用供卷能力时管理 Pod 会保持 Pending，不会退回临时目录。管理存储不能依赖尚未通过管理面初始化的业务池。两个管理 Deployment 使用单副本 Recreate，更新存在短暂管理面中断。


先创建两个 namespace 与独立凭证。以下 Secret 创建命令只用于首次安装；已有 Secret 复用或按组织流程轮换。

```bash
kubectl create namespace pulsar-operator-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f config/storage/manager/namespace.yaml
python3 -c 'import secrets; print(secrets.token_urlsafe(32), end="")' | \
  kubectl -n pulsar-operator-system create secret generic pulsar-operator-console --from-file=token=/dev/stdin
python3 -c 'import secrets; print(secrets.token_urlsafe(32), end="")' | \
  kubectl -n diskfleet-system create secret generic diskfleet-api-token --from-file=token=/dev/stdin

kubectl kustomize deploy/bundles/all > /tmp/pulsar-operator-install.yaml
# 审查渲染文件中的镜像、namespace、hostPath、RBAC 和写盘开关后安装。
kubectl apply -f /tmp/pulsar-operator-install.yaml
kubectl -n pulsar-operator-system rollout status deployment/pulsar-operator
kubectl -n diskfleet-system rollout status deployment/diskfleet-api
kubectl -n pulsar-operator-system rollout status deployment/console-gateway
```

没有节点标签时，磁盘 DaemonSet 不会开始纳管节点。统一部署要求 Pulsar Token Secret 存在；存储 API 同样要求 Token 且长度至少 24。两套 Token 不互通，网关不会自动授予权限。

```bash
kubectl -n pulsar-operator-system port-forward svc/console-gateway 8080:8080
```

打开 `http://127.0.0.1:8080/`，左侧先选择模块：

- **Pulsar**：入口 `/#/pulsar/overview`。“集群总览”显示资源统计及 Pulsar ↔ BookKeeper 关联表，“Broker 组件”与“Bookie 组件”分别管理计算和持久化组件。
- **Storage**：入口 `/#/storage/overview`。子导航提供存储总览、节点、磁盘、池、PV/PVC、审计和指南，页面显示在同一控制台内容区。进入此模块后停止 Pulsar 轮询，Pulsar 服务不可用时也可单独访问 Storage。

旧 `/#/overview`、`/#/brokers/...`、`/#/bookies/...` 书签会自动转换为 Pulsar 模块路由。

组件列表支持名称、命名空间和关联集群筛选。进入详情后通过页签查看状态、监控、配置、版本及 Pod/事件；Bookie 额外提供 Journal/Ledger。关联资源可双向跳转。受管 Bookie 的操作自动提交到父 Pulsar 的 `spec.bookkeeper`；页面分开不改变资源的生命周期所有权。独立 BookKeeper 可在 Bookie 页面创建，需要已有 ZooKeeper；当前 CRD 不支持把已有独立 BookKeeper 任意绑定给一个 Pulsar。缺失或身份不匹配的父资源显示关联异常，并禁止写操作。

从管理员凭据渠道取得对应 Token 并分别输入：Pulsar 工作区工具栏“Pulsar 凭证”，存储内容区“访问凭证”。sessionStorage 分别保存凭证，不通过嵌入消息交换。不要把 Token 放到共享 URL。网关仅 ClusterIP，外部访问由组织的 TLS/身份代理承接。当前 Token 对应管理员权限，不提供租户级授权或单点登录。

Pulsar 控制台编辑会固定打开时的资源 UID 和 resourceVersion。保存遇到并发修改时返回冲突并保留草稿，需重新加载最新内容后再合并；不会把旧表单覆盖到轮询得到的新版本。直接调用 Pulsar 控制台的 PATCH、DELETE 或 restart API 时，除 Bearer Token 外还必须提供 `X-Resource-UID` 与 `X-Resource-Version` 请求头。二者来自本次编辑基线的 `metadata`；缺少返回 428，身份或版本改变返回 409。受管 Bookie 的修改使用父 PulsarCluster 的身份。存储 API 使用下文独立的 PVC UID 参数契约。

## 5. 将物理盘接到系统并纳管

硬件安装、热插拔和控制器识别详见[物理盘手册](../../modules/storage/docs/physical-disks.md)。关键顺序是：接线/插盘 → 内核与 udev 发现 → 核对资产身份 → 创建未批准 DiskPool → 审查 → 开启写入与批准 → 等待 Ready。

```bash
# 在目标 Linux 节点执行只读检查
sudo udevadm settle
lsblk --bytes -o NAME,PATH,TYPE,SIZE,MODEL,SERIAL,WWN,FSTYPE,MOUNTPOINTS,RO
ls -l /dev/disk/by-id/
# 预先构建/分发目标 Linux 架构的 Go Agent 二进制
./bin/diskfleet-agent --discover

# 在管理机选择本批节点；worker-01 替换为真实节点名
kubectl label node worker-01 storage.diskfleet.io/enabled=true
kubectl get physicaldisks -o wide
kubectl -n diskfleet-system logs -l app=diskfleet-agent --tail=100
cp config/storage/samples/diskpool.yaml /tmp/my-reviewed-pool.yaml
```

编辑副本中的 `nodeName`、唯一 `vgName`、by-id 路径、序列号和精确字节容量；一池只允许一块物理盘，先保留 `approved: false`。也可在“物理盘”页面生成计划并审查。`Available` 只是通过软件保护检查，仍需资产负责人确认盘用途。

```bash
kubectl apply -f /tmp/my-reviewed-pool.yaml
kubectl get diskpools -o yaml
# 下面允许初始化已批准的空盘；先审查本批节点全部待处理池。
kubectl -n diskfleet-system set env daemonset/diskfleet-agent ENABLE_DISK_WRITES=true
kubectl patch diskpool worker-01-ssd-01 --type=merge -p '{"spec":{"approved":true}}'
kubectl get diskpool worker-01-ssd-01 -w
```

该开关影响所有已启用节点；按节点标签分批上线，审查此前积压的 approved 池。已有文件系统、分区、挂载、swap、RAID 签名、外部 LVM 或 holders 会被拒绝，程序不自动擦盘。不要删除保护检查来接管有数据的设备。

**盘交给 LVM 后不直接挂到 `/mnt`，也不为原始盘写 `/etc/fstab`。** Agent 建 VG，CSI 从中分配 LV 并建立 ext4，kubelet 将 PVC 对应卷挂到 Pod 的 `volumeMounts`。`/var/lib/diskfleet` 的 UUID/所有权日志与 `/etc/lvm` 需持久保留并备份。

整套部署重新 apply 会恢复默认 `ENABLE_DISK_WRITES=false`。若长期维护受控写入配置，应在自己的 overlay 明确记录；不要依赖一次 `set env` 作为不可追踪的配置。

## 6. 用本仓库 Pulsar 验证动态存储

先等待六个或更多实际 DiskPool Ready。根目录生成器只读取集群并输出计划，不应用资源，也不读取 kubeconfig；在本地通过回环 `kubectl proxy` 使用现有 kubeconfig：

```bash
# 另一个终端保持运行；不监听公网地址
kubectl proxy --address=127.0.0.1 --port=8001
```

```bash
# 用真实 Ready 池名称替换以下列表；三组节点范围必须一致。
make build-storage-plan
KUBE_API_URL=http://127.0.0.1:8001 bin/storage-plan \
  --zookeeper-pools journal-node-a,journal-node-b,journal-node-c \
  --journal-pools journal-node-a,journal-node-b,journal-node-c \
  --ledger-pools ledger-node-a,ledger-node-b,ledger-node-c \
  --namespace diskfleet-pulsar-e2e --name diskfleet-pulsar \
  --output /tmp/pulsar-storage-plan.json
```

生成器验证池 Ready、容量、真实 hostname、池 UID、每等级每节点一池以及 journal/ledger 隔离；输出专用 namespace、三个 fleet StorageClass 和一个 `PulsarCluster`。每个 StorageClass 包含精确 VG 白名单及拓扑，避免只允许一个节点的类与三副本硬反亲和冲突。

审查计划后：

```bash
# server dry-run 不会持久化 List 中的 Namespace，须先建立目标命名空间。
kubectl create namespace diskfleet-pulsar-e2e --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --dry-run=server -f /tmp/pulsar-storage-plan.json
kubectl apply -f /tmp/pulsar-storage-plan.json
kubectl -n diskfleet-pulsar-e2e get psc,bkc,pod,pvc -w
```

`test/fixtures/pulsar-storage/*example.json` 包含虚构池 UID，只供离线测试，不能直接应用。若仅演练生成，可添加 `--pools-json test/fixtures/pulsar-storage/pools.example.json`，并使用该文件的 `journal-0,1,2` 与 `ledger-0,1,2` 名称。

控制器按 ZooKeeper → BookKeeper 元数据 → BookKeeper → Pulsar 元数据 → Broker → Proxy 协调。PVC 使用 `WaitForFirstConsumer`，尚无 Pod 消费时 Pending 属正常。不要给工作负载设置 `spec.nodeName` 绕过调度器；使用 selector/affinity。

确认 `PulsarCluster.status.phase=Running` 且 `observedGeneration` 追上 generation 后，在专用测试集群执行：

```bash
TIMEOUT=600 make e2e-pulsar-storage \
  NAMESPACE=diskfleet-pulsar-e2e CLUSTER=diskfleet-pulsar
```

该命令核验至少九个业务 PVC/PV 的驱动、绑定和拓扑，使用真实 Bookie 用户写 journal/ledger 标记，发送 20 条消息，重建 bookie-0 并检查文件与消息保留，再经父 CR 扩 ledger 1 GiB、核查 UID 不变及文件系统增长，最后用独立探针验证 Retain。会改变专用测试集群，保留扩大后的卷、测试 topic/标记与 Released 探针 PV/LV；清理说明见[联合验收手册](../testing/pulsar-storage-e2e.md)。

单独验收存储用 `make e2e-storage STORAGE_CLASS=<真实单池类名>`；原上游 Pulsar 生命周期验收仍在 `test/e2e/pulsar/lifecycle.sh`，与存储联合验收不是同一测试。任何脚本失败都应保留诊断现场。

生命周期脚本从 `pulsar-operator-system/pulsar-operator-console` Secret 读取 Token，也可用 `CONSOLE_TOKEN_FILE` 指定凭据文件。`PREFLIGHT_ONLY=1 NS=<命名空间> NAME=<集群> bash test/e2e/pulsar/lifecycle.sh` 只验证认证和资源身份。完整运行会扩缩 Bookie、修改配置，仅用于专用测试集群。单池存储脚本同样支持 `PREFLIGHT_ONLY=1`；它按池 UID 查找真实对象，验证节点拓扑，并仅为已存在的控制平面 NoSchedule 污点添加容忍。

## 7. 扩容、回收与故障处理

Pulsar 创建的 PVC 在存储页面显示为外部管理对象，只能观察。修改父 CR 扩容，例如测试初始 ledger 为 20 GiB 时：

```bash
kubectl -n diskfleet-pulsar-e2e patch pulsarcluster diskfleet-pulsar --type=merge \
  -p '{"spec":{"bookkeeper":{"storage":{"ledger":{"size":"21Gi"}}}}}'
kubectl -n diskfleet-pulsar-e2e get pvc -w
```

实际值必须大于当前声明；刚执行过 E2E 时可能已经是 21 GiB，不应再套用旧例。Pulsar 控制器同步子 BookKeeper CR 与 PVC，并处理 StatefulSet 模板变化；CSI 完成卷/文件系统扩容。禁止缩容和原地改变 StorageClass。扩容前检查每个涉及池的余量。

当前在线扩容仅实现 BookKeeper 的 journal/ledger。已有 ZooKeeper 或内置 Prometheus 的卷大小、StorageClass 变更会被明确拒绝并报告条件；不会仅扩大 Prometheus 保留阈值而继续使用旧容量。此类存储调整需单独制定备份、迁移和恢复方案。

删除 PVC 与回收底层数据分开处理：默认 Retain 使 PV/LV 留存，Released 空间不会自动返还。BookKeeper decommission 会在复制清理后删除相应 PVC，即使集群删除策略为 Retain；CSI 的 Retain 仍保留底层卷。删除 DiskPool 会被保护 finalizer 阻止；不得用批量移除 finalizer 代替退役流程。

存储页面释放自建卷时会提交确认时的 `pvcUid`。直接调用存储 `DELETE /api/volumes/{id}?uid=<pvcUid>` 也应带此参数；旧 UID 的重试只恢复旧卷记录，不会删除同名新 PVC。释放意图先记录在 SQLite，网络响应丢失后可继续核对结果；API 数据目录挂载独立 `diskfleet-api-state` PVC，Pod 重建后复用。外部 Pulsar 卷始终只读。

本地盘故障不能通过把 Pod 调到另一节点就恢复同一卷。依靠 BookKeeper 副本、备份和应用恢复；新盘用新身份创建新池，不复制旧 journal 冒认。容量扩展、新节点加入 fleet 等级通常需要新 StorageClass 与业务迁移，不能修改已有 PVC 的存储类。

## 8. 运维与排障

| 现象 | 优先检查 |
|---|---|
| Agent 没有 Pod | 节点 enabled 标签、DaemonSet 选择器与特权策略 |
| 盘 Protected / 池未 Ready | by-id/serial/容量、宿主挂载/签名/holders、approved、写入开关、节点日志 |
| PVC Pending | 消费 Pod、三组拓扑交集、硬反亲和可用节点、CSI 状态、VG 余量 |
| 页面 401 | 是否输入该模块 Token、Secret 是否存在、网关 Authorization 转发 |
| 页面 502 | 两个后端 Service/Endpoints、就绪状态、nginx 日志；网关 healthz 仅验证本身 |
| 扩容不生效 | 父 CR observedGeneration、事件、StorageClass allowVolumeExpansion、池容量与 CSI resizer |
| 管理 API Pending 或审计不可用 | 检查管理 PVC、StorageClass、卷挂载权限和剩余容量；保持原状态卷及单副本 Recreate 配置 |

```bash
kubectl -n pulsar-operator-system logs deployment/pulsar-operator --tail=100
kubectl -n diskfleet-system logs deployment/diskfleet-api --tail=100
kubectl -n pulsar-operator-system logs deployment/console-gateway --tail=100
kubectl -n diskfleet-pulsar-e2e get events --sort-by=.metadata.creationTimestamp
kubectl describe pvc -n diskfleet-pulsar-e2e YOUR_PVC
```

大规模上线按机房/机架分批，保留盘序列号与槽位映射、审批记录及初始化日志；接入磁盘健康与容量监控。当前每节点独立调谐能并行工作，但中央 API 仍全量扫描、缓存并单副本运行，每盘也产生一个 StorageClass。watch 索引、分片、持久审计和真实故障压测是后续规模化工作；模拟万盘元数据测试不能替代生产规模验收。
