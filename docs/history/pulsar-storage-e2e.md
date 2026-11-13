# Pulsar / Storage 联合验收历史记录

> 以下保留特定镜像的实验结果与当时的环境状态，不代表当前版本的验收结论。当前操作流程见[验收指南](../testing/pulsar-storage-e2e.md)。


日志为运行机器上的 `/root/workspace/pulsar-deployment-evidence/shared-e2e.log`，完整证据目录为 `/root/workspace/pulsar-deployment-evidence/diskfleet-pulsar-e2e.WnikXAer/`。独立复核得到：

- 9 个业务 PVC Bound，均对应 `local.csi.openebs.io`、Retain PV；全部原 PVC/PV UID 与绑定保持。
- bookie-0 的 journal/ledger 标记文件由镜像用户 `uid=10000(pulsar), gid=0` 写入，Pod 重建后同用户读回；原 20 条消息生产和消费成功。
- 三个 ledger 实际容量均为 21Gi；bookie-0 的文件系统从 20466256 KiB 增至 21498384 KiB，来自同一 VG/LV 挂载路径。
- 独立 1Gi 探针 PV `pvc-0d567184-691a-4d14-9408-9237367358b7` 保持原 UID、Retain，状态 Released；未删除业务 PVC。

本次业务镜像为 `apachepulsar/pulsar:4.0.14`，LVM CSI chart/driver 为 1.10.1。当前控制台入口是 `http://172.23.254.149:30080` / `http://172.23.254.150:30080`。获取两个独立管理 Token、已开启写盘状态下的镜像升级步骤见[两节点指南](../operations/two-node.md#7-控制台与功能验证)。Agent 当前 `ENABLE_DISK_WRITES=true`，初始发现 installer 会拒绝覆盖；按升级指南分别更新 workload，保留状态，不重新初始化盘或回写原 20Gi 声明。

本验收不证明故障盘自动恢复、节点丢失后的本地数据迁移、机架故障容忍或大规模性能。BookKeeper 的副本机制与 DiskFleet 本地 PV 的故障域需要分别验证。NodePort 的 HTTP/API 与静态资源检查不等于浏览器 DOM、布局及交互测试；本次未执行这些浏览器测试。
