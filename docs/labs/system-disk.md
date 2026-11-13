# 两节点系统盘目录联调

这是新增数据盘前准备的替代方案。用户随后明确改用两块数据盘，当前业务已按[共享数据盘指南](shared-data-disk.md)部署。本页的系统盘版 Pulsar 清单未应用；此前仅部署 local-path provisioner，并验证两个测试卷的文件保留与跨节点网络，相关测试资源保留。

本替代配置复用现有 Kubernetes，用 Rancher Local Path Provisioner 为每个 PVC 建立独立目录，不创建 LVM PV/VG，不格式化系统盘，不修改 fstab，也不将目录伪装成 DiskFleet 物理池。以下方法供无数据盘环境参考，不要覆盖当前数据盘部署；当前结果见[验证报告](../history/validation.md)。

## 资源与数据路径

| 项目 | 配置 |
|---|---|
| Pulsar 清单 | [deploy/labs/system-disk/pulsar.yaml](../../deploy/labs/system-disk/pulsar.yaml) |
| 存储部署入口 | [deploy/labs/system-disk/storage](../../deploy/labs/system-disk/storage/kustomization.yaml) |
| 业务 namespace / PulsarCluster | `diskfleet-pulsar-system-lab` / `pulsar-lab` |
| StorageClass | `pulsar-system-lab`，`WaitForFirstConsumer`、`Retain`、非默认类，不启用扩容 |
| provisioner 标识 | `pulsar.songof.dev/system-lab` |
| provisioner namespace | `pulsar-system-lab-storage`，独立安装 Rancher Local Path Provisioner v0.0.37 |
| 宿主数据根目录 | 两节点均为 `/var/lib/pulsar-system-lab` |
| 组件 | 3 ZooKeeper、3 Bookie、2 Broker、1 Proxy、1 AutoRecovery |
| 镜像 | `apachepulsar/pulsar:4.0.14` |
| 调度 | 所有组件限定 `pulsar.songof.dev/two-node=true`，Preferred 反亲和，仅容忍控制平面/master 的 NoSchedule taint |
| BookKeeper | `minReplicas: 3`；Broker ensemble/write/ack quorum 为 `2/2/2` |
| 监控 | 本 profile 不创建 Prometheus，避免额外占用系统盘 |

每个 ZooKeeper 申请 1Gi，每个 Bookie 的 journal 申请 1Gi、ledger 申请 5Gi；共 **9 个 PVC，声明容量合计 21Gi**。这些请求只是 Kubernetes 对象中的容量声明，**local-path 不提供目录容量配额**。多个 PVC 共享同一宿主文件系统，单个目录可超过声明值；21Gi 也不代表已经预留或隔离了 21Gi。官方明确说明不强制容量上限。[Rancher 官方说明](https://github.com/rancher/local-path-provisioner#cons)

业务稳态 requests 合计 **20.5Gi 内存、3.3 CPU**，还需给系统、Operator、provisioner、初始化 Job 与镜像解压留余量。Preferred 不保证 2+1 分布，两节点本地目录也没有跨节点复制；本配置不提供主机故障高可用。

```text
节点系统文件系统
  /var/lib/pulsar-system-lab/
    每个 PV/PVC 的独立目录
      ↓ local-path 创建 PV 并绑定实际节点
    Pulsar PVC → Pod volumeMount
```

PV 绑定后数据在所属节点。节点故障时，把 Pod 调到另一台不会自动迁移这些文件。ZooKeeper/journal/ledger 的独立 PVC 也不代表物理盘、I/O 或故障域隔离。

## 部署前检查

1. 两个节点都必须 Ready，CNI 和 DNS 正常。按实际 Node 名核对两节点标签，拒绝额外节点也带 `pulsar.songof.dev/two-node=true`。
2. Pulsar Operator、存储发现 API/Agent、统一网关已按[两节点指南](../operations/two-node.md)部署；磁盘 Agent 保持 `ENABLE_DISK_WRITES=false`。本方案不要求 OpenEBS LVM CSI 或 Ready DiskPool。
3. `pulsar-system-lab` StorageClass 已由本实验的 local-path provisioner 安装，配置为 Retain/WFFC。provisioner 与 helper Pod 的调度范围、taint 容忍必须覆盖这两节点，`nodePathMap` 只允许已核实节点的 `/var/lib/pulsar-system-lab`。
4. 数据子目录由 provisioner 创建为 UID 10000、GID 0、权限 0770，与当前 Pulsar 镜像用户及 Pod `fsGroup=0` 配合。不要递归修改系统盘其他目录的权限。
5. 在两台宿主分别检查 `df -h /var/lib`、`df -i /var/lib`、`du -sh /var/lib/pulsar-system-lab`（目录已存在时），确认系统空间与 inode 有足够余量。镜像、containerd、etcd 和业务目录共用系统盘；只做小流量联调，持续监控剩余空间。

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
kubectl get nodes -o wide
kubectl get nodes -l pulsar.songof.dev/two-node=true -o wide
kubectl get storageclass pulsar-system-lab -o yaml
kubectl -n pulsar-system-lab-storage get deploy,pod -o wide
kubectl -n pulsar-operator-system get deploy,pod
```

StorageClass 中 provisioner 必须是 `pulsar.songof.dev/system-lab`，不能使用默认类或其他生产类代替。实际目录名称和节点以创建后的 PV 为准，不凭 PVC 名猜测路径。

首次安装本实验的目录 provisioner 时，先审查存储入口和来源记录，再执行：

```bash
kubectl apply -k deploy/labs/system-disk/storage
kubectl -n pulsar-system-lab-storage rollout status deployment/pulsar-system-lab-provisioner --timeout=5m
```

此部署关闭自动目录删除，即便后续误变更 PV 回收策略，清理脚本也不会直接递归删除目录；实际清理仍须由管理员按 PV 身份和保留要求处理。

## 创建 Pulsar

从仓库根目录执行。先建立 namespace，后续 server dry-run 才能验证其中的自定义资源；dry-run 的 Namespace 本身不会持久化。

```bash
kubectl create namespace diskfleet-pulsar-system-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --dry-run=server -f deploy/labs/system-disk/pulsar.yaml
kubectl apply -f deploy/labs/system-disk/pulsar.yaml
kubectl -n diskfleet-pulsar-system-lab get psc,bkc,pod,pvc -o wide
kubectl -n diskfleet-pulsar-system-lab wait pulsarcluster/pulsar-lab \
  --for=jsonpath='{.status.phase}'=Running --timeout=20m
kubectl -n diskfleet-pulsar-system-lab get pulsarcluster pulsar-lab \
  -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
```

确认两个 generation 相同、所有业务组件 Ready、9 个 PVC 全部 Bound。WFFC 下没有消费者时 PVC Pending 属于正常阶段；若业务长期 Pending，检查 Pod/PVC Events、helper Pod、provisioner 日志和节点空间，不通过删除 PVC 或降低副本数绕过错误。

```bash
kubectl -n diskfleet-pulsar-system-lab get pvc \
  -o custom-columns=NAME:.metadata.name,STATUS:.status.phase,PV:.spec.volumeName,CLASS:.spec.storageClassName,REQUEST:.spec.resources.requests.storage
kubectl get pv -o wide
kubectl -n diskfleet-pulsar-system-lab get events --sort-by=.metadata.creationTimestamp
```

逐个核对实际 PV 的 provisioner 注解、`hostPath.path` 或 `local.path`、`nodeAffinity`、Retain 策略以及 claimRef UID 与 PVC 的对应。目录应位于已批准根路径下，绑定节点应与使用该 PVC 的 Pod 一致。该验收对象不是 CSI PV，因此不要求也不伪造 `spec.csi.driver`。

## 控制台与消息联调

```bash
kubectl -n pulsar-operator-system port-forward svc/console-gateway 8080:8080
```

浏览器访问 `http://127.0.0.1:8080`。Broker 和 Bookie 仍是独立组件视图，受管 Bookie 关联父 `diskfleet-pulsar-system-lab/pulsar-lab`，配置写入父 PulsarCluster。Pulsar 详情可以检查业务 PVC；物理存储页面继续显示真实设备发现结果，不把系统目录标为 Ready LVM 池。

在另一个终端进行小批量消息测试，先创建订阅，再发送与消费：

```bash
RUN_ID=$(date +%s)-$RANDOM
TOPIC="persistent://public/default/system-disk-smoke-$RUN_ID"
SUB="system-disk-smoke-$RUN_ID"
kubectl -n diskfleet-pulsar-system-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-admin topics create-subscription "$TOPIC" -s "$SUB" -m earliest
kubectl -n diskfleet-pulsar-system-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-client --url pulsar://localhost:6650 produce "$TOPIC" -m "system-disk-$RUN_ID" -n 20
timeout 120s kubectl -n diskfleet-pulsar-system-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-client --url pulsar://localhost:6650 consume "$TOPIC" -s "$SUB" -n 20
```

记录 20 条发送成功、20 条接收成功及匹配的消息内容。这会留下实验 topic/subscription。只验证启动和消息成功，不等于验证故障恢复；需要 Pod 重建实验时，先记录 PVC/PV UID、目录文件与消息，再针对单个 Bookie 重建，确认同一 PVC/PV 和原数据恢复后另行记录结果。

## 能证明什么，哪些仍未验证

此 profile 可验证 Operator 调谐、组件创建、Bookie 关联、统一页面、动态目录 PV/PVC、容器目录权限和真实消息路径。它**不验证**空盘身份保护、DiskPool 审批、LVM PV/VG/LV 创建、OpenEBS CSI、独立物理 journal/ledger、CSI 挂载和扩容、大规模物理盘管理性能。

local-path 没有本项目 LVM CSI 扩容能力；本 StorageClass 不启用 `allowVolumeExpansion`。不要通过修改 PVC/PSC 的 size 来声称目录卷扩容成功，也不要对本类执行 `make e2e-storage` 或 `make e2e-pulsar-storage`：这些脚本要求真实 DiskFleet/LVM CSI，后者还要求三节点隔离。本清单不带 `storage.diskfleet.io/integration=pulsar` 或虚构 pool UID。

## 保留、迁移与清理

BookKeeper 的 PVC reclaim policy 和 StorageClass 的 PV reclaim policy 均为 Retain。删除业务对象不等于自动释放宿主目录空间；保留的 PV、PVC 和目录应按实际对象逐项核实。Bookie decommission 流程也可能删除 PVC，Retain PV 仍保留数据。不要删除 namespace 后直接递归清空 `/var/lib/pulsar-system-lab`。

后续拿到独立数据盘时，按[物理盘手册](../../modules/storage/docs/physical-disks.md)创建真实 Ready DiskPool、安装并验证 LVM CSI，建立新的物理存储 Pulsar 集群，再通过应用级迁移或已验证的备份恢复切换。已有 PVC 的 StorageClass 不能直接改成 LVM 类，移动目录或创建同名 PV 也不等于完成数据迁移。
