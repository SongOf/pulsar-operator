# Storage 测试

```bash
modules/storage/scripts/dev.sh unit
modules/storage/scripts/dev.sh integration
modules/storage/scripts/dev.sh test
```

`unit` 运行资源类型、HTTP 处理、Kubernetes fake client、SQLite、规划器和启动参数的 Go 测试。`integration` 运行 `test/integration/storage/http_workflow_test.go`，验证跨 HTTP、存储与重启的工作流，再执行页面模板检查和存储 E2E 前置检查夹具。

Storage→Agent 资源契约归系统集成，由 `make test-system` 调用。真实卷读写、重建、扩容与 Retain 使用 `e2e CLASS`。本地基准 `benchmark --disks 10000` 衡量元数据处理，不代表真实磁盘 IOPS 或集群规模。

初始化方式见[使用说明](usage.md)，完整分层、依赖和执行边界见[仓库测试指南](../../../docs/testing/README.md)。
