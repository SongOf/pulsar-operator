# 两节点系统目录联调存储

此目录适用于已明确选择系统盘目录联调的 `.149/.150` 环境。完整部署见 [系统盘联调指南](../../../docs/system-disk-lab.md)。入口为 `kubectl apply -k deploy/system-disk-lab/storage`；不要用 `kubectl apply -f` 扫描本目录，因为 `source.json` 是来源记录。

基于官方 [Rancher local-path-provisioner v0.0.37](https://github.com/rancher/local-path-provisioner/releases/tag/v0.0.37)，原始清单 SHA256 与本地调整记录保存在 [source.json](source.json)。运行独立的 `pulsar.songof.dev/system-lab` provisioner，仅处理显式指定 `pulsar-system-lab` StorageClass 的 PVC。

- namespace：`pulsar-system-lab-storage`；Deployment：`pulsar-system-lab-provisioner`。
- 只允许 `k8s-control-1`、`iz6weeh0nfgmlsr99i8atmz`，且节点需标记 `pulsar.songof.dev/two-node=true`。
- 每个 PVC 在所属节点的 `/var/lib/pulsar-system-lab/` 下创建独立目录。目录为 UID 10000/GID 0、权限 0770，以供 Pulsar 容器写入。
- StorageClass 为 `Retain`、`WaitForFirstConsumer`，不设默认类，不支持扩容。自动目录删除脚本被禁用，回收需逐个审核数据和对应 PV。
- **PVC 标称容量不构成实际配额。** 所有目录共享各自节点的系统文件系统；此实现没有物理盘隔离、LVM、CSI、跨节点复制或文件系统配额。

`smoke.py` 在新建的专用 namespace 中创建两个 1Mi PVC 和两个非 root Pod，分别调度到两节点。它验证真实写读、双向跨节点 Service DNS/HTTP，以及 Pod UID 改变后同一 PVC/PV 的文件保留。重建 Pod 不会重新写入标记，所以不会把数据丢失误判为持久化成功。

```bash
python3 deploy/system-disk-lab/storage/smoke.py
```

脚本保留 namespace、Pod、Service、PVC、PV 和很小的标记文件，证据默认写到 `/root/workspace/pulsar-deployment-evidence/`。它不删除 PVC/PV，不验证删除后的 Released/Retain 生命周期，也不执行物理盘或 CSI 验收。再次运行会创建新的唯一测试 namespace，应按输出记录管理保留资源。
