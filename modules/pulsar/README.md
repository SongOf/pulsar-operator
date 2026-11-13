# Pulsar 模块

通过 `PulsarCluster` 与 `BookKeeperCluster` 管理 Pulsar 集群生命周期，并提供控制台 HTTP API、用户操作与控制器关键动作审计。

`cmd/pulsar` 创建 controller-runtime Manager，注册 Kubernetes 类型、控制器和 HTTP 服务。`internal/pulsar/controller` 根据 CR 调谐集群，`resources` 构造工作负载，`httpapi` 提供查询及声明式变更，`metrics` 汇总监控数据，`audit` 持久保存请求、资源变更和关键状态。部署资源属于 `config/pulsar`；公用前端由根 `web` 构建并随本模块镜像提供。

在仓库根运行：

```bash
modules/pulsar/scripts/dev.sh init --integration
web/scripts/dev.sh init
web/scripts/dev.sh build
modules/pulsar/scripts/dev.sh build
modules/pulsar/scripts/dev.sh test
modules/pulsar/scripts/dev.sh render > /tmp/pulsar.yaml
```

`run` 启动控制器并连接所选 Kubernetes 集群；`image` 构建镜像，`deploy` 显式安装模块，`generate` / `manifests` 更新类型辅助代码和清单。具体参数、凭证配置与 CR 使用见[使用说明](docs/usage.md)，实现流程见[设计说明](docs/design.md)，测试见[测试说明](docs/testing.md)，功能状态见[运维范围](docs/operator-scope.md)，操作追溯见[审计说明](docs/audit.md)。
