# 物理盘接入、分配与挂载手册

本文仓库内命令默认从 `pulsar-operator/modules/storage/` 执行。统一部署与镜像构建见[仓库说明](../../../README.md)，物理盘相关路径仍指 Linux 宿主机上的真实设备。

DiskFleet 的真实路径是 **物理盘 → LVM Physical Volume → VG → CSI 创建 LV → Kubernetes PV/PVC → 容器文件系统**。一块物理盘对应一个 DiskPool/VG，避免把多盘故障影响混在同一个池内。这里 LVM 的 PV 是 Physical Volume，Kubernetes 的 PV 是 PersistentVolume，两者不是同一对象。

当前默认使用厚分配（`thinProvision: "no"`），不启用容量超卖。由 Operator 创建并保护 VG，OpenEBS LVM CSI 负责逻辑卷、文件系统和挂载。**原始盘加入 LVM 后不需要挂到 `/mnt`，也不需要为原始盘写 `/etc/fstab`。**不要在交给 CSI 管理的设备上另外运行格式化或手工挂载操作。

## 1. 接入前的硬件条件

- **SAS/SATA**：确认背板、电源、线缆和控制器支持该磁盘；只有设备与主板/背板都支持热插拔时才在线插拔，否则停机安装。SAS 通常经 HBA 暴露磁盘；SATA 可经 AHCI 或 HBA 连接。
- **NVMe**：确认 PCIe 插槽/背板支持相应协议及热插拔；普通 M.2 不应假定支持在线拔插。安装后应出现 `nvmeXnY` 命名空间及对应稳定设备身份。
- **HBA/JBOD**：本项目的原始盘纳管要求操作系统能看到独立磁盘及稳定序列号。HBA 的 IT/JBOD 模式较适合这种布局。不要将已有数据的 RAID 成员盘交给本项目初始化，也不要为迁就项目在线改变启动控制器的 RAID/AHCI 模式。
- **硬 RAID、软件 RAID、multipath**：当前版本不自动拆解、创建或接管这些存储栈。存在分区、文件系统、RAID 签名、LVM、挂载、swap 或 holders 时拒绝当作新空盘纳管。故障域与冗余由集群设计决定。
- **操作系统**：Linux 节点安装 LVM2、util-linux；具备 device-mapper 支持。部署 CSI 还需要符合其版本要求的 Kubernetes/kubelet 环境。安全策略必须允许节点 Agent 的宿主设备访问和 CSI 的挂载权限。

插盘后先等待内核和 udev 识别：

```bash
sudo udevadm settle
lsblk --bytes -o NAME,PATH,TYPE,SIZE,MODEL,SERIAL,WWN,FSTYPE,MOUNTPOINTS,RO
ls -l /dev/disk/by-id/
sudo journalctl -k -n 100 --no-pager
```

若 SCSI/SAS 控制器未自动发现新盘，先按厂商说明确认热插拔支持和正确的 `hostX`。必要时仅扫描对应控制器，例如以下命令中的 `hostX` 必须替换为已经核实的目标控制器：

```bash
printf '%s\n' '- - -' | sudo tee /sys/class/scsi_host/hostX/scan
sudo udevadm settle
```

这是发现新设备的操作，不是磁盘初始化。不要批量遍历所有控制器，也不要在生产节点试验移除已有设备。NVMe 使用 PCIe/厂商提供的发现机制，不使用上面的 SCSI 命令。

## 2. 先只读检查身份和用途

在项目目录运行：

```bash
python3 scripts/inspect-disks.py
```

输出 `Available` 才表示通过当前自动安全检查，仍需核对物理槽位/资产记录与序列号。`Protected` 会列出阻止纳管的原因；**不要删除这些检查来绕过保护**。原始业务数据未必带已知文件系统签名，签名扫描不能证明盘上没有有价值的数据，必须确认磁盘用途。

以下命令均为只读诊断；先把 `DEVICE` 设置为已核实的完整 by-id 路径：

```bash
DEVICE=/dev/disk/by-id/REPLACE_WITH_VERIFIED_ID
readlink -f "$DEVICE"
sudo udevadm info --query=property --name="$DEVICE"
sudo lsblk --bytes -o NAME,PATH,TYPE,SIZE,SERIAL,WWN,FSTYPE,MOUNTPOINTS,RO "$DEVICE"
sudo wipefs --no-act --json "$DEVICE"
findmnt --real
cat /proc/swaps
sudo pvs --reportformat json
sudo vgs --reportformat json
```

Agent 同时检查 `/proc/1/mountinfo`、`/proc/swaps`、`/sys/class/block/*/holders` 以及子设备。DaemonSet 配置 `hostPID: true`，使 `/proc/1/mountinfo` 对应宿主挂载命名空间；仅查看容器内的挂载点不足以保护宿主磁盘。

序列号、精确字节容量和 by-id 路径共同构成审批身份。`/dev/sdb` 等枚举名称重启后可能变化，因此不接受它作为初始化目标。by-id 缺失时应修复 udev/控制器身份上报，不能仅手工伪造一个名字绕过检查。盘换了但槽位没变也必须重新纳管，不能沿用旧盘身份。

## 3. 发现 → 审批 → 创建池

部署准备见根目录 README。以下步骤假定 CRD、节点 Agent 和 OpenEBS LVM CSI 已正确安装，且工作节点上需要的镜像已可用。

1. 仅选择计划纳管的节点，设置 `storage.diskfleet.io/enabled=true` 标签。默认 Agent 的 `ENABLE_DISK_WRITES=false`，此时先采集 PhysicalDisk 信息。
2. 在前端或 `kubectl get physicaldisks -o yaml` 中核对节点、by-id、序列号、容量和保护原因。
3. 前端生成接管计划，查看每块设备、容量和命令后勾选确认；API 提交时同时校验计划 ID 与确认文本。也可以基于 `deploy/example-pool.yaml` 创建 `approved: false` 的 DiskPool。
4. 实际初始化前，在部署配置中明确开启 `ENABLE_DISK_WRITES=true`，再批准已经核实的 DiskPool。两项条件缺少任一项都不会初始化磁盘。下面的 DaemonSet 环境变量命令作用于所有已启用节点；执行前必须审查这些节点所有已批准但未执行的池，防止积压请求同时开始。分批上线时先用节点标签限定本批范围。
5. 查看 DiskPool 状态；只有 `Ready` 才表示 VG 和该池的 StorageClass 已就绪。CSI 驱动是否可用仍需实际 PVC/Pod 验证。

管理命令示例中的对象名应替换为现场已核实的值：

```bash
kubectl label node worker-01 storage.diskfleet.io/enabled=true
kubectl get physicaldisks -l storage.diskfleet.io/node=worker-01 -o yaml
kubectl get diskpools -l storage.diskfleet.io/node=worker-01 -o yaml

# 填写复制出来的示例文件中的实际节点、by-id、serial、sizeBytes；先保持 approved:false。
kubectl apply -f my-reviewed-pool.yaml

# 以下两条会使已经批准的空盘初始化请求具备执行条件；只在审查完对应计划后执行。
kubectl -n diskfleet-system set env daemonset/diskfleet-agent ENABLE_DISK_WRITES=true
kubectl patch diskpool worker-01-ssd-01 --type=merge -p '{"spec":{"approved":true}}'
kubectl get diskpool worker-01-ssd-01 -o yaml
```

初始化只会创建经过身份核验的 LVM PV 与 VG，不自动擦除已有签名，也不使用 `--force`。当前一次只处理一盘，批量纳管应生成多个独立 DiskPool，而不是跨节点或多盘合并一个 VG。节点进程使用文件锁串行化磁盘初始化，重复 reconcile 不重复创建已有池。

`/var/lib/diskfleet/<vgName>.json` 是持久化所有权记录，包含设备身份、预先生成的 LVM PV UUID 和 VG 标签。它允许在 `pvcreate` 或 `vgcreate` 后进程中断时核对并继续。丢失记录不会自动认领原有 VG。这个目录必须存放在持久宿主路径，并纳入安全备份；它不是业务数据备份。

## 4. 从磁盘池到容器挂载

Operator 为池创建带节点约束的 StorageClass，配置 `local.csi.openebs.io`、`WaitForFirstConsumer`、`Retain` 和 `allowVolumeExpansion: true`。先创建 PVC，直到存在使用它的 Pod 才会选择节点并分配。不要把“没有消费者时 PVC Pending”当成故障，也不要用 `spec.nodeName` 绕过调度器；需要限定节点时使用节点选择器或 affinity。

CSI 在已纳管 VG 中创建 LV，首次供文件系统卷使用时创建 ext4，并通过 kubelet 的卷挂载流程将它提供给容器。Pod 只需要声明 PVC 和 `volumeMounts`，示例见 `deploy/example-workload.yaml`。该示例的 StorageClass 必须改为对应池真实生成的类名。

查询完整链路：

```bash
kubectl get diskpools,storageclasses
kubectl get pvc,pod -n YOUR_NAMESPACE -o wide
kubectl get pv
kubectl describe pvc YOUR_PVC -n YOUR_NAMESPACE
kubectl describe pod YOUR_POD -n YOUR_NAMESPACE
```

在节点上可只读查看 `pvs`、`vgs`、`lvs -a`、`lsblk -f`、`findmnt`。不要在 CSI 挂载点之上再次手工挂载，也不要通过编辑 `/etc/fstab` 管理这些业务 LV。

## 5. 真实 Kubernetes 验收

`scripts/e2e-k8s.sh` 只使用你明确传入的现有 DiskFleet StorageClass。它不安装驱动、不改节点、不创建 VG，也不删除 PV/LV。

```bash
bash scripts/e2e-k8s.sh diskfleet-worker-01-ssd-01
# 镜像拉取较慢时可增大每一步超时；离线集群可指定已同步的 busybox 兼容镜像。
TIMEOUT=300 TEST_IMAGE=YOUR_REGISTRY/busybox:1.37.0 \
  bash scripts/e2e-k8s.sh diskfleet-worker-01-ssd-01
```

脚本预先核验真实 StorageClass 的 CSI 驱动、Retain、延迟绑定和扩容能力，然后创建唯一 namespace。验证 2 GiB PVC 绑定、实际读写和 sync、Pod 重建后数据保持、扩到 3 GiB 后文件系统容量增长、删除测试 Pod/PVC 后 PV 进入 Released 且仍为 Retain。扩容验证包含一次重新挂载，不宣称验证了所有驱动的在线扩容行为。

每一步都有超时；失败时保留集群资源，打印诊断信息并保留本地诊断目录。成功后仍保留 3 GiB 的 PV/LV 和测试数据，并打印对象名称。**脚本的成功表示这条测试链路通过，不代表节点掉电、盘损坏、恢复演练或大规模压力验证已经通过。**

当前已在两节点数据盘环境完成真实 CSI 与 Pulsar 联合验收，详见[统一验证报告](../../../docs/validation.md)。单元测试与模拟演示仍不能替代新环境的实际调度、挂载和恢复验证。可先用 `PREFLIGHT_ONLY=1` 运行上述单池脚本，只检查池 UID、实际节点和调度条件，不分配卷。

## 6. 回收、故障与替换

- **删除 PVC**：默认 Retain 保留 PV 和背后的 LV，容量不会自动返还。Released PV 不会自动分配给另一个业务。先确认旧业务、数据保留期和恢复需求，再由管理员制定重新绑定或销毁流程。
- **删除 DiskPool**：保留 finalizer 防止把“删除管理对象”误当成“销毁数据”。先盘点 PVC、PV、LV 和业务备份，再决定是否移除管理记录；不要直接批量移除 finalizer。
- **Agent 重启**：保留 `/var/lib/diskfleet`。Operator 通过 UUID/标签/设备身份确认已管理 VG，继续同步状态。不要删除 journal 后尝试重新初始化。
- **VG/PV 消失或身份改变**：自动停止该池操作，保留日志和磁盘现场。检查连接、HBA、udev、LVM 元数据备份以及系统日志；不要先运行 `pvcreate`、`vgcreate` 或 `wipefs` 尝试“修好”。存储恢复需要针对原始设备和元数据进行人工评估。
- **换盘**：同一槽位的新盘不是旧盘。用新身份创建独立池，业务数据由备份或应用复制恢复，完成迁移后再退役旧池。无需也不应复制旧 journal 去冒认新盘。
- **节点或磁盘故障**：本地 PV 的数据在所属节点，普通单盘 LVM 没有跨节点副本。不能仅把 Pod 调度到另一台机器就恢复数据。需要故障切换的业务应采用应用级复制、独立备份恢复，或选择本身提供复制的存储方案。

Retain 测试完成后，可以删除脚本输出的空 namespace；保留的 PV、LV 和数据需要单独处理。不要把 `kubectl delete pv` 等同于保证回收磁盘空间；Kubernetes 对象和底层 LVM 数据生命周期必须一起核对。本项目不提供自动破坏性清盘入口。

## 7. 大规模运维边界

按机房、机架、节点分批纳管，保存序列号与槽位对应关系，并对节点写入开关、审批、操作日志和 journal 备份进行审计。先用少量节点跑完真实 CSI 验收与故障恢复演练，再扩大批次；不要同时对所有机器初始化磁盘。

每节点独立协调磁盘，避免全局串行瓶颈；控制状态刷新频率与批次大小。SMART/NVMe 健康、温度、介质错误、VG 剩余容量以及 CSI 错误应接入监控系统。当前 UI 的分页/筛选和批量计划是运维基础能力，并不等于已经通过数万节点的性能验证。正式规模承诺需要测量 API Server/etcd 负载、对象数量、控制器队列、CSI 并发、节点重启和磁盘故障时的恢复时间。

参考：[Kubernetes StorageClass 与延迟绑定](https://kubernetes.io/docs/concepts/storage/storage-classes/)、[OpenEBS LVM StorageClass 参数](https://openebs.io/docs/user-guides/local-storage-user-guide/local-pv-lvm/configuration/lvm-storageclass-parameters)、[CSI 部署与宿主挂载要求](https://kubernetes-csi.github.io/docs/deploying.html)。
