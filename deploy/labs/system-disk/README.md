# 系统盘目录实验配置

此目录只用于两个指定节点上的目录卷联调，与真实 LVM 数据盘验收分开。

- [storage](storage/README.md)：独立命名的 local-path provisioner、Retain StorageClass 和来源记录。
- [pulsar.yaml](pulsar.yaml)：使用该 StorageClass 的实验 PulsarCluster；不会被管理面或 storage 的 Kustomization 自动创建。
- 探针实现：`test/e2e/system/system-disk-smoke.py`；命令入口 `scripts/test.sh e2e-system-disk`。

完整操作见[系统目录实验指南](../../../docs/labs/system-disk.md)。目录卷不证明 LVM/CSI、扩容或物理容量配额有效。
