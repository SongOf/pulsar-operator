# 项目文档

根 README 介绍项目功能；各模块 README 介绍模块设计和基本使用。详细文档按所属模块存放，跨模块架构、部署与测试保留在本目录。

目录分工：`design` 保存整体设计，`operations` 保存组合部署，`labs` 保存实验环境说明，`testing` 保存测试方法，`history` 保存历史验收，`provenance` 保存来源记录。模块自己的设计、使用和测试说明仍留在 `modules/<模块>/docs`。

部署清单导航见 [deploy](../deploy/README.md)，可执行命令见 [scripts](../scripts/README.md)。

| 主题 | 入口 |
| --- | --- |
| Pulsar | [模块文档](../modules/pulsar/docs/README.md) |
| Storage | [模块文档](../modules/storage/docs/README.md) |
| Agent | [模块文档](../modules/agent/docs/README.md) |
| 公用前端 | [设计与开发](../web/docs/README.md) |
| 开发规范 | [Codex 仓库约定](../AGENTS.md) |
| 整体设计 | [架构](design/architecture.md) / [项目组织](design/repository.md) |
| 环境与命令 | [脚本说明](../scripts/README.md) / [部署使用指南](operations/deployment.md) |
| 测试 | [分层与入口](testing/README.md) / [Pulsar + Storage E2E](testing/pulsar-storage-e2e.md) |
| 实验环境 | [两节点部署](operations/two-node.md) / [共享数据盘](labs/shared-data-disk.md) / [系统目录实验](labs/system-disk.md) |
| 源码来源 | [来源说明](provenance/README.md) / [导入清单](provenance/upstream-source.json) |
| 历史证据 | [整合版本验证](history/validation.md) / [审查修复记录](history/review-fixes.md) |

历史验证保留当时的镜像、语言与测试计数；不据此推定当前 Go 版本已通过目标环境的真实磁盘验收。
