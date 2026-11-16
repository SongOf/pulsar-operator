# Storage 模块（DiskFleet）

提供磁盘库存、接管计划与审批、存储池、PVC 管理和审计 API，以及 Pulsar 存储规划和本地元数据基准工具。实际设备操作由独立 Agent 执行。

`cmd/storage-api` 启动 HTTP 服务；`internal/storage/httpapi` 负责请求与鉴权，`cluster` 管理 Kubernetes 资源，`store` 保存 SQLite 状态，`fleet` 生成规划清单。Kubernetes 类型属于 `api/storage/v1alpha1`，部署资源属于 `config/storage`。存储与 Pulsar 页面共用 `web/src` React 前端，API 从 `web/dist` 提供构建产物；镜像包含同一套页面。

在仓库根运行：

```bash
modules/storage/scripts/dev.sh init
web/scripts/init.sh
web/scripts/build.sh
modules/storage/scripts/dev.sh test
modules/storage/scripts/dev.sh run
```

默认在 `http://127.0.0.1:8080` 启动模拟环境，状态保存在 `modules/storage/.state`。模拟模式不操作真实磁盘。

集群模式的节点列表显示全部 Kubernetes Node，包括尚未部署 Agent 的节点。页面通过启用节点标签触发已安装的 Agent DaemonSet，节点详情展示磁盘库存和成功扫描有效期。只有节点健康、Agent 运行、设备可用且扫描未过期时才能提交磁盘接管计划；提交时后端再次核对。

`build` 构建 API、规划和基准工具；`plan --help` 查看规划参数，`benchmark --disks 10000` 执行本地元数据基准；`image` 构建镜像，`render` 输出 Storage 清单。真实部署、连接配置及使用步骤见[使用说明](docs/usage.md)，实现见[设计说明](docs/design.md)，接口契约见[API 文档](docs/api.md)，其余资料见[文档目录](docs/README.md)。
