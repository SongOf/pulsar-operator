# 两节点共享数据盘联调

本次用户已明确选择使用两台节点上的 100Gi 数据盘，并授权初始化这两块已核实的设备。对应配置为 **`two-node-shared-disk-lab`**：每节点一盘一 VG，ZooKeeper、Bookie journal 和 ledger 从同一个 VG 分配各自的 LV/PV/PVC。该配置验证真实 DiskFleet/LVM/CSI 数据路径，但三个角色共用物理盘，不提供物理盘或 I/O 隔离，也不保证主机故障高可用。

**2026-11-13（北京时间）已在 149/150 完成真实部署及共享盘 E2E。** `diskfleet-pulsar-data-lab/pulsar-lab` 的 PulsarCluster、BookKeeperCluster 均已 Running，3 ZooKeeper、3 Bookie、2 Broker、1 Proxy、1 AutoRecovery 就绪，9 个业务 PVC Bound；3 个 ledger 已从 20Gi 扩至 **21Gi**。统一控制台为 `http://172.23.254.149:30080` 或 `http://172.23.254.150:30080`，从可达该私网的客户端访问。

本页后面的生成、初始化和安装步骤用于首次部署参考。现有集群已完成这些步骤，不应重新初始化磁盘、重建业务 PVC 或回写初始 20Gi 配置。运行证据与当前升级说明分别见下文和[两节点指南](../operations/two-node.md)。

## 三个 profile 的区别

| profile | 节点与物理盘 | 反亲和 | 原三节点 E2E |
|---|---|---|---|
| `three-node-e2e`（默认） | 至少三节点，每节点 journal/ledger 独立池 | Required | 满足其拓扑前提后可执行 |
| `two-node-lab` | 恰好两节点，每节点 journal/ledger 独立池 | Preferred | 不适用 |
| `two-node-shared-disk-lab` | 恰好两节点，三个角色选择同一组两个池，每节点一盘 | Preferred | 不适用 |

允许 journal/ledger 共盘仅限第三个显式 profile，原来的两种配置仍拒绝该布局。新配置使用独立 `storage.diskfleet.io/profile` 和 `storage.diskfleet.io/integration=pulsar-two-node-shared-disk-lab` 标签，StorageClass 名也与原 profile 区分；不通过添加 `integration=pulsar` 绕过原三节点验收限制。

## 本次设备与池的对应

| 节点 | Kubernetes Node 名 | 稳定设备路径 | DiskPool | VG |
|---|---|---|---|---|
| 172.23.254.149 | `k8s-control-1` | `/dev/disk/by-id/virtio-6wei56fkuf85q5njbpip` | `lab-data-149` | `df_lab_data_149` |
| 172.23.254.150 | `iz6weeh0nfgmlsr99i8atmz` | `/dev/disk/by-id/virtio-6weiwwldjhukty6wc5sp` | `lab-data-150` | `df_lab_data_150` |

两盘序列号分别为 `6wei56fkuf85q5njbpip`、`6weiwwldjhukty6wc5sp`，核实容量均为 **107374182400 字节（100Gi）**，当前内核路径均是 `/dev/vdb`。稳定身份由 by-id、serial 和精确容量共同校验，不能只凭 `/dev/vdb` 执行初始化。系统盘 `/dev/vda` 不属于本授权。

只读盘点和最初设备审核记录在运行环境的 `/root/workspace/pulsar-deployment-evidence/inventory-current.json`、`data-disks-review.json`；后续明确授权保存在 `data-disks-authorization.json`，其中 `approved:true` 对应“改用这两块数据盘”。两块盘已通过 Agent 初始化为上述 VG，两个 DiskPool Ready。物理检查的 `physical-pool-validation.json` 核对了 by-id、serial、PV UUID、VG ownership、9 个业务 LV 与系统盘未加入 LVM；该文件中的 LV 容量是扩容前快照，扩容后容量以本次 E2E 和实时对象为准。

初始化由磁盘 Agent 执行，先持久化 PV UUID 和 VG ownership tag，再执行相应 LVM 命令；不应手工裸建 VG 后要求 Agent 接管。保留 `/var/lib/diskfleet` 的所有权日志以支持幂等重试和中断恢复。数据盘加入 LVM 后不需要挂到 `/mnt` 或写 fstab，后续 LV、文件系统和 Pod 挂载由 OpenEBS LVM CSI 负责。

## 容量和组件

初次部署时，3 ZooKeeper 各申请 5Gi，3 Bookie 各申请 journal 5Gi、ledger 20Gi，合计 9 个 PVC、90Gi 业务声明容量。验收后 ledger 均为 21Gi，业务声明总量为 **93Gi**，另保留 1Gi Retain 探针。Preferred 反亲和不能保证 2+1 分布，因此生成初始计划时**每个共享池都必须有至少 94Gi 实际可用容量**，而非只检查盘的标称容量：

| 每个池的保守预算 | 容量 |
|---|---:|
| 3 个 ZooKeeper 卷 | 15Gi |
| 3 个 Bookie journal 卷 | 15Gi |
| 3 个 Bookie ledger 卷 | 60Gi |
| ledger 每副本扩 1Gi 的余量 | 3Gi |
| 独立 Retain 探针余量 | 1Gi |
| 合计 | **94Gi** |

100Gi 数据盘通常能容纳这份预算，但生成器以 Ready 池的 `availableBytes` 为准，不依赖标称容量推断。93Gi 或比 94Gi 少一个字节均拒绝生成；额外测试卷、已有 LV 或业务扩容必须重新核算空间。现有业务已经占用空间，重新运行初始生成器可能因不足 94Gi 空闲而被拒绝，这不表示运行中的卷异常。

业务组件保持 3 ZooKeeper、3 Bookie、2 Broker、1 Proxy、1 AutoRecovery，BookKeeper `minReplicas=3`，Broker ensemble/write/ack quorum 为 `2/2/2`。稳态 requests 合计 **20.5Gi 内存、3.3 CPU**，未包含 Kubernetes、两个管理模块、CSI 和初始化 Job。所有组件及其 metadata Job 都通过 `pulsar.songof.dev/two-node=true` 限定节点，并容忍 control-plane/master 的 NoSchedule taint。

三个角色仍使用三个逻辑 StorageClass，但精确 VG 白名单和池 UID 集合相同。分开的是 LV/PVC，不是物理盘；StorageClass 保持厚分配、ext4、Retain、WaitForFirstConsumer 与允许扩容。

## 从真实 Ready 池生成清单

先按[两节点指南](../operations/two-node.md)完成管理面、两节点标签与 OpenEBS LVM CSI 安装。初始化完成后核对：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
kubectl get nodes -l pulsar.songof.dev/two-node=true -o wide
kubectl get diskpool lab-data-149 lab-data-150 -o yaml
kubectl get csidriver local.csi.openebs.io
kubectl get csinode -o yaml
```

两个 DiskPool 都需 Ready，实际 UID、VG、hostname 与各自节点匹配且空间满足上述预算；两个 CSI node plugin 与 controller 也需 Ready。尚未完成初始化时，不使用 fixture 伪造 Ready 状态。

在另一个终端开启仅监听 loopback 的 Kubernetes 代理：

```bash
KUBECONFIG=/etc/kubernetes/admin.conf kubectl proxy --address=127.0.0.1 --port=8001
```

从仓库根目录生成新清单，三个角色必须传入完全相同的两个池：

```bash
KUBE_API_URL=http://127.0.0.1:8001 ./bin/storage-plan \
  --profile two-node-shared-disk-lab \
  --zookeeper-pools lab-data-149,lab-data-150 \
  --journal-pools lab-data-149,lab-data-150 \
  --ledger-pools lab-data-149,lab-data-150 \
  --namespace diskfleet-pulsar-data-lab --name pulsar-lab \
  --output /tmp/pulsar-shared-data-plan.json
```

该命令只读，不初始化磁盘、不执行 apply；输出已存在时拒绝覆盖。live 模式还要求恰好两个带标签、Ready、非封锁且未删除的节点；每个存储角色的 pool.node 集合必须等于该节点集合，池 hostname 必须与实时节点标签一致。

审核清单后，先建立专用 namespace，再执行服务端校验和安装：

```bash
kubectl create namespace diskfleet-pulsar-data-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --dry-run=server -f /tmp/pulsar-shared-data-plan.json
kubectl apply -f /tmp/pulsar-shared-data-plan.json
kubectl -n diskfleet-pulsar-data-lab get psc,bkc,pod,pvc -o wide
kubectl -n diskfleet-pulsar-data-lab wait pulsarcluster/pulsar-lab \
  --for=jsonpath='{.status.phase}'=Running --timeout=20m
kubectl -n diskfleet-pulsar-data-lab get pulsarcluster pulsar-lab \
  -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
```

不要将现有目录卷的 PVC StorageClass 直接改成新类；已有数据不会随新集群自动迁移。本次系统盘版 `diskfleet-pulsar-system-lab` 清单从未部署；此前两个目录测试卷保留在 `pulsar-system-storage-smoke-20261112-191201`，与当前业务卷独立。

## 已完成的真实验收

入口是仓库根目录的 `test/e2e/system/pulsar-shared-disk.sh`，使用独立 `two-node-shared-disk-lab` 保护条件。实际日志为 `/root/workspace/pulsar-deployment-evidence/shared-e2e.log`，完整证据目录为该目录下的 `diskfleet-pulsar-e2e.WnikXAer/`。

| 检查 | 本次结果 |
|---|---|
| 真实 CSI 卷 | 9 个业务 PVC Bound；实际 PV 均为 `local.csi.openebs.io`、Retain，节点亲和性匹配挂载 Pod |
| Bookie 文件持久化 | journal/ledger 两个标记文件由 `uid=10000(pulsar), gid=0` 写入，bookie-0 重建后仍以同一用户读回 |
| 消息持久化 | 发送 20 条，重建 bookie-0 后消费原 20 条，检查数量与测试标记 |
| 在线扩容 | 通过父 PulsarCluster 将全部 ledger 从 20Gi 扩至 21Gi；bookie-0 的 `df` 从 20466256 KiB 增至 21498384 KiB |
| 对象身份 | 全部 9 个原 PVC/PV UID 和绑定不变 |
| Retain 生命周期 | 独立 1Gi 探针 Pod/PVC 删除后，原 PV `pvc-0d567184-691a-4d14-9408-9237367358b7` 保持原 UID、Retain，并进入 Released |

保留的探针 PV/LV、topic/subscription 和 `.diskfleet-e2e-133d4af9d5f1` 文件是有意留下的证据，不自动清理或缩容。可以先只读重检：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
PREFLIGHT_ONLY=1 TIMEOUT=600 bash test/e2e/system/pulsar-shared-disk.sh \
  diskfleet-pulsar-data-lab pulsar-lab
```

去掉 `PREFLIGHT_ONLY=1` 会再次重建 bookie-0、把每个 ledger **再增加 1Gi**，并额外留下一个 1Gi Retain 探针；先核算当前两个 VG 的剩余空间。只读预检通过不等于重新执行了全部 E2E。

## 控制台与现有集群维护

访问上面的任一 NodePort，Pulsar 和内嵌存储页面使用两个独立 Token。在有集群权限的私有终端按登录需要读取，命令仅显示在指南中，仓库和验收报告不记录 Token 值：

```bash
kubectl --kubeconfig=/etc/kubernetes/admin.conf -n pulsar-operator-system \
  get secret pulsar-operator-console -o jsonpath='{.data.token}' | base64 -d
kubectl --kubeconfig=/etc/kubernetes/admin.conf -n diskfleet-system \
  get secret diskfleet-api-token -o jsonpath='{.data.token}' | base64 -d
```

两节点入口的静态资源、HTTP/API、独立鉴权、Pulsar→BookKeeper 真实关联及 9 个只读业务卷展示已检查，证据为 `console-http-validation.json`。这不是浏览器 DOM、布局或点击流程测试。物理盘发现中 LVM 元数据别名优先级的修复已纳入实现；运行中的最终发现状态见[验证记录](../history/validation.md)，不把早期快照中的历史 Offline CR 当作新的物理故障。

本次数据盘管理已经启用 `ENABLE_DISK_WRITES=true`。`scripts/deploy/two-node.sh install` 属于初始只读发现安装入口，会拒绝覆盖已开启写入的 Agent。升级时 Operator 可使用 `kubectl set image`，首次将 Storage API / Agent 从 Python 切换为 Go 必须分别同时 patch image 与 command；保留现有开关、节点选择、Secrets、业务对象及宿主 `/var/lib/diskfleet` 所有权日志；完整命令见[两节点指南](../operations/two-node.md#8-现有部署的镜像升级)。

## 验证边界

Running 后确认 observedGeneration 与 generation 相等、全部组件 Ready、9 个 PVC Bound。逐个检查实际 PV 的 `local.csi.openebs.io` driver、Retain、claimRef UID、NodeAffinity，以及 Pod 挂载路径和所用节点。记录实际 VG/LV、可用容量与 CSI 事件，不能只凭 StorageClass 对象宣称真实卷已创建。

通过统一控制台核对 Broker/Bookie 关联以及物理存储页的两池/外部只读业务卷。小批量消息验证可沿用[两节点指南的消息步骤](../operations/two-node.md#7-控制台与功能验证)，将业务 namespace 改为 `diskfleet-pulsar-data-lab`；需实际确认发送、接收数量和内容匹配。

单独的存储验收 `make e2e-storage STORAGE_CLASS=...` 可针对已核实的 LVM 类执行，但会额外创建并保留 3Gi 测试 LV，必须先按当时剩余空间核算，不能把上表 1Gi 保留探针预算当成完整存储脚本的空间预算。原 `make e2e-pulsar-storage` 明确要求三节点及 journal/ledger 物理隔离，不适用于本 profile；本次通过的是上面的独立共享盘入口。

角色共盘意味着一个节点上的该盘故障会同时影响该节点的 ZooKeeper、journal 和 ledger。节点故障、磁盘损坏、跨节点恢复、生产级吞吐或大规模物理盘管理仍需另行验证。后续增加独立 journal/ledger 盘时应生成新的双盘 profile，并制定业务数据迁移，不直接更改既有 PVC 的 StorageClass。

## 离线参考与回归

[shared-disk-pools.example.json](../../test/fixtures/pulsar-storage/shared-disk-pools.example.json) 的 UID、Ready 状态、节点和 VG 均为虚构，仅用于源码审查及测试，不能应用到真实集群。

```bash
go test ./internal/storage/fleet -count=1
```

回归覆盖旧 profile 保持行为、新 profile 100Gi 可用与 94Gi 边界、93Gi 拒绝、三角色必须同池、身份/VG 白名单保护、live 节点检查、离线标识和拒绝覆盖输出。
