# Agent 设计与实现

Agent 是独立 DaemonSet 进程。`cmd/agent` 解析节点、状态目录、调谐间隔与写盘开关，建立 Kubernetes 客户端，循环执行本节点调谐；退出信号会取消上下文。`--discover` 单独调用设备发现，不建立 Kubernetes 连接。

`discovery` 收集 Linux 块设备、稳定 by-id 路径、serial、容量、挂载和已有签名。`controller` 发布 PhysicalDisk，读取本节点 DiskPool，并复核 spec 节点归属和设备身份。通过核验的初始化请求交给 `lvm`；新池的临时 LV 压测结果与分配审核状态分别保存，VG 创建成功不等于允许分配业务卷。

写入要求 `ENABLE_DISK_WRITES=true` 且池 `approved=true`。系统盘、挂载盘、swap、分区、RAID、holders、外部 PV/VG 或缺失身份信息会被拒绝。每个 VG 只包含一块盘，CSI 负责其中业务 LV 的生命周期。

LVM 操作使用宿主 `.lvm.lock` 串行化，并将中间阶段保存在 `<vgName>.json`。日志中的 poolUID、PV UUID、VG tag 和设备身份用于恢复与归属核验，不能仅凭 VG 名称接管已有数据。缺少 poolUID 的 Ready 日志只有在池、StorageClass、PV UUID、VG tag 和单盘成员关系全部核验后才能补齐身份，不会重新初始化设备。

Agent 与 Storage 无直接 HTTP/RPC 调用。二者共享 `api/storage/v1alpha1` 和 `internal/kubeconfig`；Kubernetes 保存期望状态和观测结果。跨模块资源契约由系统集成测试覆盖，设备和 LVM 行为由本模块测试覆盖。

每次成功扫描到的磁盘会更新 `PhysicalDisk.status.lastObservedTime`、`validUntil` 和 `nodeUID`，使管理 API 能区分当前库存与失联节点留下的旧记录。有效期为配置间隔（最低 10 秒）的 1.2 倍，涵盖调谐随机抖动；默认间隔 30 秒对应 36 秒。扫描失败或 Node 身份无法确认时不续期；消失的磁盘标为 Offline 并保留上次成功观察时间。扫描前后 Node UID 变化时拒绝发布该轮结果。

控制台部署 Agent 使用预装 DaemonSet 的节点选择标签，不直接创建临时 Pod，也不打开写盘开关。新 Node 对象不能复用旧 Node UID 的磁盘观察记录来创建池。

## 临时 LV 压测

`lvm.Backend.EnsureBenchmark` 接收池、节点及设备身份和固定运行 ID，不接受用户自定义 fio 参数或目标路径。它持有同一个宿主 `.lvm.lock`，核验初始化日志中的 poolUID、PV UUID、VG tag、单盘成员关系，并额外固定 VG UUID。开始前必须没有业务或未知 LV，且有至少 1 GiB 空闲容量；只允许在本次新建的线性临时 LV 上读写，不格式化、不挂载它。执行器是阻塞调用，应与发现扫描分开运行。

固定配置 `temporary-lv-v1` 使用 direct I/O、libaio、单 job/thread、目标队列深度 16 和 1 GiB 区域，按顺序运行：

| 工作负载 | 块大小 | 运行时间 |
| --- | --- | --- |
| 顺序写 `seq-write` | 1 MiB | 30 秒 |
| 顺序读 `seq-read` | 1 MiB | 30 秒 |
| 随机写 `rand-write` | 4 KiB | 30 秒 |
| 随机读 `rand-read` | 4 KiB | 30 秒 |

指标包括实际处理字节数、实际运行时长、B/s、IOPS、错误计数，以及纳秒单位的平均、p95、p99 总延迟。fio 使用 `lat_ns` 和 `lat_percentiles=1`，总延迟包含提交与完成两段；不把 completion latency 与 total latency 混合。四项测量共约 120 秒，另外有设备核验、创建和清理开销；执行上限为 3 分钟，取消后的独立清理最多 30 秒。短时采样不能证明全盘健康、SSD 稳态性能、掉电持久性或 Pulsar 业务性能，也不保证达到指定队列深度或吞吐。

临时 LV 使用随机名称和 tag。创建意图先写日志并同步，再执行 `lvcreate`；创建后用有 10 秒期限的 `udevadm settle` 等待宿主事件，读取并持久保存实际 LV UUID。每项 fio 前核验设备、LV UUID/tag、块设备拓扑及未挂载/未打开状态；目标采用包含 VG/LV UUID 的稳定 by-id 路径，并禁止 fio 创建缺失文件。发现分区、陌生 holder、业务 LV、挂载或无法确定的设备状态时停止。

命令输出最多保留 1 MiB，不将 stderr 或任意命令错误正文写入状态。Linux 命令使用独立进程组和父进程退出信号，fio 以线程运行；取消后先确认进程组结束，再考虑清理。失败时保留之前完成的指标，以及失败命令输出中结构完整、可验证的部分样本；缺失或损坏的指标不会伪造为零值。失败结果不能被批准分配。

## 压测日志与恢复

压测日志位于 `STATE_DIR/benchmarks/<SHA-256(poolUID)>/<runID>.json`，与既有 `<vgName>.json` 分开。日志文件权限为 0600，目录为 0700，写入采用文件同步、原子 rename 和目录同步。记录包含池/节点/设备身份、PV/VG/LV UUID、归属 tag、阶段、指标、清理证据和结果摘要。时间以 UTC 整秒序列化，摘要在保存完成时间后生成，跨重启和时区解码保持稳定。

同一池 UID 的运行 ID 固定。成功或失败结果会重放，不会每次调谐重测；进程在未完成阶段退出时，新进程记录中断并只尝试恢复清理。若创建已成功但 LV UUID 尚未写回，只能由原 VG UUID、预定随机名称和唯一 tag 找到本次 LV，先补存 UUID，再清理。删除使用 UUID、VG UUID 和 tag 的组合选择器；删除后再次核验身份并检查整个 LVM 清单，防止同名替换或 LV 移动造成误判。

初始化、压测及临时 LV 删除均受写盘开关、批准状态和实时身份守卫限制。取消请求本身不会跳过清理，但撤回批准、关闭写盘开关、身份变化或 IO 进程未确认退出时保留 `cleanup-pending`，不强行删除。授权恢复且同一设备/LV 身份核验通过后，只重试清理。只有四项指标有效且 `cleanupVerified=true` 的成功结果才有资格提交人工审核。成功结果重放不要求 VG 永久为空，因此后续合法业务 LV 不会触发清理或重新压测。
