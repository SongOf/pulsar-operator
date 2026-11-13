# Pulsar / Storage 跨模块测试夹具

四份 JSON 是虚构池 UID 的离线数据，用于规划器 CLI 集成检查和文档演练，不能直接应用到真实集群。

| 文件 | 用途 |
| --- | --- |
| [pools.example.json](pools.example.json) | 默认三节点、journal/ledger 分盘输入 |
| [two-node-pools.example.json](two-node-pools.example.json) | 两节点分盘实验输入 |
| [shared-disk-pools.example.json](shared-disk-pools.example.json) | 两节点每节点一块数据盘的实验输入 |
| [manifest.example.json](manifest.example.json) | 默认规划输出示例，供审查和回归比较 |

真实清单应从当前 Ready DiskPool 生成。运行 `modules/storage/scripts/run.sh plan --help` 查看规划参数，完整步骤见[集成指南](../../../modules/storage/docs/pulsar-integration.md)。

原上游字段观察记录已移到[来源契约](../../../docs/provenance/pulsar-storage-contract.json)。规划器包自身的旧格式兼容夹具仍在 `internal/storage/fleet/testdata`，它与此处 CLI 示例承担不同测试职责。
