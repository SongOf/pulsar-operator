# 测试入口

包内 Go 单元测试与源码放在一起，公用前端单测位于 `web/src`。

| 目录 | 内容 |
| --- | --- |
| `integration/pulsar` | Pulsar 客户端脚本检查；envtest 保留在 controller/httpapi 包中 |
| `integration/storage` | Storage HTTP/持久化工作流与脚本前置检查 |
| `integration/system` | 跨模块契约、网关、部署组合、工具入口和联合验收前置检查；入口 `run.sh` |
| `e2e/pulsar`、`e2e/storage`、`e2e/system` | 分模块与联合真实集群验收 |
| `e2e/lib` | E2E 公共客户端与实现 |
| `fixtures/pulsar-storage` | 跨模块规划器离线输入和输出示例 |
| `support` | 仅测试使用的工具夹具 |

各模块使用 `modules/<模块>/scripts/dev.sh unit` / `integration`；总入口使用 `make test-unit` / `test-integration` / `test-system`。完整要求见[测试指南](../docs/testing/README.md)。
