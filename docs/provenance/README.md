# 源码来源

Pulsar 控制器的上游为 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator)，固定基线为 [`3c042c57a79e83cd3f329c7088a65e2bc65a62af`](https://github.com/SongOf/pulsar-operator/commit/3c042c57a79e83cd3f329c7088a65e2bc65a62af)。项目保留 `github.com/SongOf/pulsar-operator` Go module 和上游声明。

[upstream-source.json](upstream-source.json) 记录 59 个导入文件的原始路径、当前本地路径、来源 URL 和导入摘要。`importedSha256` 对应导入时捕获的内容；读取器曾归一化空行，因此它不是原始 Git blob 的摘要。上游 README 见 [upstream-readme.md](upstream-readme.md)。

`localModifications` 记录本地内容差异的原因和当前 SHA-256。修改来源文件时同步维护对应条目；移动文件只调整 `localPath`，不改原始路径、URL 或导入摘要。仓库测试会校验文件与清单一致。

[pulsar-storage-contract.json](pulsar-storage-contract.json) 保存对固定上游版本的字段、PVC 名称及存储行为的观察依据；它不是运行时配置，验证范围以文件中的版本和限制为准。当前跨模块输入输出示例位于[测试夹具](../../test/fixtures/pulsar-storage/README.md)。

发布前按[测试指南](../testing/README.md)验证目标版本，并遵守上游、依赖与镜像各自的许可。
