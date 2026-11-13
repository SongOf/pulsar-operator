# Agent 测试

```bash
modules/agent/scripts/dev.sh unit
modules/agent/scripts/dev.sh integration
modules/agent/scripts/dev.sh test
```

`unit` 运行发现、设备保护、日志兼容、恢复与文件锁测试；设备与命令执行使用夹具。`integration` 启动 envtest，加载 Storage CRD，验证资源 schema、状态更新和 resourceVersion 冲突，设备操作仍使用模拟器。

Storage→Agent 契约属于系统集成。Linux、CSI 和真实设备行为通过 Storage/联合 E2E 另行验收。

初始化方式见[使用说明](usage.md)，完整分层、依赖和执行边界见[仓库测试指南](../../../docs/testing/README.md)。
