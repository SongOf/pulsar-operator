# Agent 使用说明

本地开发依赖 Go；运行设备发现和节点调谐需要 Linux、LVM2、util-linux、udev、fio 及目标设备访问权限。镜像已提供这些 Linux 工具。

```bash
modules/agent/scripts/dev.sh init --integration
modules/agent/scripts/dev.sh build
modules/agent/scripts/dev.sh test
modules/agent/scripts/dev.sh render > /tmp/agent.yaml
AGENT_IMG=YOUR_REGISTRY/pulsar-operator-agent:YOUR_RELEASE modules/agent/scripts/dev.sh image
```

`init` 只下载 Go 依赖，`--integration` 额外准备 envtest。`render` 不连接集群。默认 `config/agent/default` 只包含 DaemonSet、ServiceAccount 和 RBAC，依赖 Storage 提供的 namespace、PhysicalDisk/DiskPool CRD。`scripts/dev.sh render storage` 输出完整 Storage + Agent 组合。

| 配置 | 作用 |
| --- | --- |
| `NODE_NAME` / `--node-name` | 本实例负责的节点；调谐模式必填 |
| `STATE_DIR` | 宿主初始化/压测恢复日志与锁目录，默认 `/var/lib/diskfleet` |
| `RECONCILE_INTERVAL` | 调谐间隔，默认 30 秒，最小 10 秒 |
| `ENABLE_DISK_WRITES` | 默认关闭；只有值为 true（忽略大小写）且池已批准才允许初始化、临时 LV 压测及清理 |
| `KUBE_API_URL` 等连接变量 | ServiceAccount 以外的显式 Kubernetes 连接配置，见共享[连接实现](../../../internal/kubeconfig/config.go) |

在目标 Linux 节点执行 `modules/agent/scripts/dev.sh discover` 只读发现设备，不访问 Kubernetes。`run` 运行持续调谐，会连接集群并上报资源；实际 DaemonSet 使用构建后的二进制，无需在节点安装 Go。

统一控制台的物理节点页面可为健康节点添加启用标签，触发已安装 DaemonSet 的部署。Agent 每轮成功扫描后更新磁盘成功观察时间和有效期；默认有效期 36 秒，覆盖一个间隔及最大随机抖动。上报过期、缺失或 Node 身份变化时，管理页面和 API 禁止新增磁盘接管请求，等待成功扫描恢复。

`modules/agent/scripts/dev.sh deploy` 显式应用模块清单，不包含批准磁盘或开启写入的步骤。节点选择、审批与 CSI 准备见[物理盘手册](../../storage/docs/physical-disks.md)。

新池通过设备核验后，Agent 在无业务 LV 的 VG 中创建专用 1 GiB 临时 LV，执行顺序写、顺序读、随机写、随机读各 30 秒。压测配置固定为 `temporary-lv-v1`，不提供任意设备路径或 fio 参数输入。容量不足、已有业务 LV 或设备保护检查失败都会阻止压测。短时结果仅描述此配置下的采样，不代表全盘检测或真实业务性能。

查看 DiskPool 的 `status.benchmark` 可获得各项指标、阶段、错误码、运行 ID、结果摘要和 `cleanupVerified`。初始化批准不等于分配批准：压测成功且临时 LV 清理已核验后，才可人工审核对应结果。失败时已完成阶段及可解析的部分测量仍会保留；未测出的数据不会记为零。

保留并备份 `STATE_DIR` 的全部内容，包括 `<vgName>.json` 和 `benchmarks/`。同一池的压测不会因重启或重复调谐重新执行。遇到 `Interrupted`、`Failed` 或未核验清理时，先检查池/节点 UID、设备身份和日志；不要按 LV 名称手工删除，不要删除日志来触发重测。批准撤回、写盘开关关闭或身份不明时自动清理也会停止。重新核实原任务身份并恢复授权后，Agent 仅尝试清理本次临时 LV，不重新执行压测。恢复机制与局限见[设计说明](design.md#压测日志与恢复)。
