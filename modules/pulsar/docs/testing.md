# Pulsar 测试

```bash
modules/pulsar/scripts/dev.sh unit
modules/pulsar/scripts/dev.sh integration
modules/pulsar/scripts/dev.sh test
```

`unit` 运行 `api/pulsar`、`internal/pulsar` 和 `cmd/pulsar` 的普通 Go 测试，包括审计持久化、失败阻断、脱敏、游标分页与请求关联。`integration` 运行控制器与 HTTP 变更前置条件的 envtest 测试，验证名称 admission、用户请求到控制器执行的审计链路，以及 `test/integration/pulsar` 的认证客户端脚本检查。`test` 依次执行两者。

控制器测试主动驱动资源状态：envtest 不提供 kubelet 或集群控制器，因此 Pod/Job/StatefulSet 状态由测试推进。真实集群生命周期验收通过 `e2e` 显式调用。

初始化方式见[使用说明](usage.md)，完整分层、依赖和执行边界见[仓库测试指南](../../../docs/testing/README.md)。
