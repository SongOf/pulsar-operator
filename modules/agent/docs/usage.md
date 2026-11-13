# Agent 使用说明

本地开发依赖 Go；运行设备发现和节点调谐需要 Linux、LVM2、util-linux、udev 及目标设备访问权限。镜像已提供这些 Linux 工具。

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
| `STATE_DIR` | 宿主恢复日志与锁目录，默认 `/var/lib/diskfleet` |
| `RECONCILE_INTERVAL` | 调谐间隔，默认 30 秒，最小 10 秒 |
| `ENABLE_DISK_WRITES` | 默认关闭；只有值为 true（忽略大小写）且池已批准才允许初始化 |
| `KUBE_API_URL` 等连接变量 | ServiceAccount 以外的显式 Kubernetes 连接配置，见共享[连接实现](../../../internal/kubeconfig/config.go) |

在目标 Linux 节点执行 `modules/agent/scripts/dev.sh discover` 只读发现设备，不访问 Kubernetes。`run` 运行持续调谐，会连接集群并上报资源；实际 DaemonSet 使用构建后的二进制，无需在节点安装 Go。

`modules/agent/scripts/dev.sh deploy` 显式应用模块清单，不包含批准磁盘或开启写入的步骤。节点选择、审批与 CSI 准备见[物理盘手册](../../storage/docs/physical-disks.md)。
