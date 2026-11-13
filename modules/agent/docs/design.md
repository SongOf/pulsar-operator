# Agent 设计与实现

Agent 是独立 DaemonSet 进程。`cmd/agent` 解析节点、状态目录、调谐间隔与写盘开关，建立 Kubernetes 客户端，循环执行本节点调谐；退出信号会取消上下文。`--discover` 单独调用设备发现，不建立 Kubernetes 连接。

`discovery` 收集 Linux 块设备、稳定 by-id 路径、serial、容量、挂载和已有签名。`controller` 发布 PhysicalDisk，读取本节点 DiskPool，并复核 spec 节点归属和设备身份。通过核验的批准请求交给 `lvm`；成功后发布 StorageClass 与池状态。

写入要求 `ENABLE_DISK_WRITES=true` 且池 `approved=true`。系统盘、挂载盘、swap、分区、RAID、holders、外部 PV/VG 或缺失身份信息会被拒绝。每个 VG 只包含一块盘，CSI 负责其中业务 LV 的生命周期。

LVM 操作使用宿主 `.lvm.lock` 串行化，并将中间阶段保存在 `<vgName>.json`。日志中的 poolUID、PV UUID、VG tag 和设备身份用于恢复与归属核验，不能仅凭 VG 名称接管已有数据。缺少 poolUID 的 Ready 日志只有在池、StorageClass、PV UUID、VG tag 和单盘成员关系全部核验后才能补齐身份，不会重新初始化设备。

Agent 与 Storage 无直接 HTTP/RPC 调用。二者共享 `api/storage/v1alpha1` 和 `internal/kubeconfig`；Kubernetes 保存期望状态和观测结果。跨模块资源契约由系统集成测试覆盖，设备和 LVM 行为由本模块测试覆盖。
