# 后端模块

三个模块共用根 Go module，分别运行、构建镜像和部署。`cmd/<入口>` 负责启动，`internal/<模块>` 保存实现，`api/<模块>` 保存 Kubernetes 类型，`config/<模块>` 保存部署资源。本目录按模块维护 Dockerfile、脚本、设计和使用文档。

| 模块 | 启动入口 | 资源契约 | 文档 |
| --- | --- | --- | --- |
| Pulsar | `cmd/pulsar` | `api/pulsar/v1alpha1` | [模块说明](pulsar/README.md) / [详细文档](pulsar/docs/README.md) |
| Storage | `cmd/storage-api`、`cmd/storage-plan`、`cmd/storage-benchmark` | `api/storage/v1alpha1` | [模块说明](storage/README.md) / [详细文档](storage/docs/README.md) |
| Agent | `cmd/agent` | 复用 Storage 类型 | [模块说明](agent/README.md) / [详细文档](agent/docs/README.md) |

各模块使用 `modules/<模块>/scripts/dev.sh`，提供 `init`、`build`、`run`、`unit`、`integration`、`test`、`image`、`render`、`deploy`。运行 `--help` 查看模块专属命令。脚本自行定位仓库，可从任意工作目录调用；用户传入的相对文件路径仍以调用者目录为准。

公用前端与其脚本位于 [web](../web/README.md)，仓库组合脚本位于 [scripts](../scripts/README.md)。根 Makefile 转发这些入口；测试分层见[测试指南](../docs/testing/README.md)。

模块 `scripts` 内按 `init.sh`、`build.sh`、`run.sh`、`test.sh`、`deploy.sh` 拆分；Pulsar/Storage 额外维护 `generate.sh`，`dev.sh` 只分发命令。职责和直接调用方式见[脚本组织说明](../scripts/README.md)。Storage 不再保留 integrations 目录：虚构输入归[测试夹具](../test/fixtures/pulsar-storage/README.md)，来源契约归根文档。
