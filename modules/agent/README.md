# Agent 模块

在每个选定 Linux 节点运行一个 Agent，发现物理盘、上报 PhysicalDisk，并根据 DiskPool 的批准状态与设备身份准备单盘 LVM VG 和 StorageClass。CSI 继续负责业务 LV、格式化、挂载与扩容。

`cmd/agent` 负责配置与调谐循环；`internal/agent/discovery` 读取设备状态，`controller` 管理 Kubernetes 资源与调谐，`lvm` 管理操作锁及恢复日志。Agent 复用 `api/storage/v1alpha1`，通过 Kubernetes 与 Storage 协作，没有面向 Console 的 HTTP 服务。

在仓库根运行：

```bash
modules/agent/scripts/dev.sh init --integration
modules/agent/scripts/dev.sh build
modules/agent/scripts/dev.sh test
modules/agent/scripts/dev.sh render > /tmp/agent.yaml
# 在目标 Linux 节点执行只读发现：
modules/agent/scripts/dev.sh discover
```

默认 `ENABLE_DISK_WRITES=false`。`config/agent/default` 只安装 Agent，依赖 Storage 的 namespace 和 CRD；`deploy/bundles/storage` 组合两者。环境变量、部署及运行方法见[使用说明](docs/usage.md)，设备核验与恢复机制见[设计说明](docs/design.md)，其他文档见[文档目录](docs/README.md)。
