# 源码来源与整合记录

## 上游基线

Pulsar 控制器来自用户指定的公开仓库 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator)，本次读取修订为 [`3c042c5`](https://github.com/SongOf/pulsar-operator/commit/3c042c5)。导入 Go 控制器/API/测试、React 页面、CRD/RBAC/部署、构建与原始 E2E 共 59 个当前文件；旧版 `api/v1` 与根目录演示前端等已被该修订删除的文件没有复活。

最初因执行环境网络限制，源码通过远程读取工具逐文件获取，每文件检查编号行 `0..N-1` 完整，再以 LF 和结尾换行保存。读取器会归一化部分空行；记录的 SHA-256 是**导入时捕获内容**的摘要，不是上游 Git blob 校验值。

2026-11-13 已通过 Git 拉取并核实完整基线 `3c042c57a79e83cd3f329c7088a65e2bc65a62af`，恢复上游历史，整合分支 `integration/storage-console` 基于该提交继续开发。原导入摘要保留：59 个文件中 17 个与 Git 内容逐字节归一化后匹配，其余捕获内容不作为原始 blob 的承诺。当前修改另有摘要和完整回归证据。

[upstream-source.json](upstream-source.json) 保存原始路径、当前初始落点、来源 URL、行数和导入时摘要。上游 README 保存到 [upstream-readme.md](upstream-readme.md)，项目根 README 改为整合版入口。导入清单不随之后的本地修改覆盖，因此可识别已改动文件。

已改动文件另记在 `localModifications`，包含原因和当前 SHA-256；移动后的上游 README 修正了相对链接。后续修改这些导入文件时同步更新该列表，保留原 `importedSha256`。仓库测试会核对这两类摘要，避免未经记录的源码漂移。

## 本次新增与修改

- 将先前独立 DiskFleet 项目纳入 `modules/storage`，保留 `diskfleet` Python 包名和 `storage.diskfleet.io/v1alpha1` API，避免改名造成已有资源不兼容。
- 保留 Pulsar Go 的标准目录和 `github.com/SongOf/pulsar-operator` 模块路径，未复制第二套 Pulsar 控制器。
- 增加 `deploy/all`、`deploy/console`、仓库级命令、测试与文档；重构 Pulsar 为总览、Broker、Bookie、物理存储四个管理入口，存储页面在同源内容区嵌入并支持子路径。
- 为管理 API 增加完整 BookKeeper 清单，严格核验父子 UID、API group、命名空间、同名子资源和唯一 controller 引用；失配资源保持只读，Pod 与事件读取按组件/资源身份区分。
- CI 覆盖两个模块、统一部署渲染和两个镜像构建；当前工作流不自动发布镜像。
- 原独立工作目录保留，本仓库是本次整合交付物；提交保留真实上游历史，不使用无关根提交覆盖远端。

## 后续合并与发布

后续使用正常 fetch、分支合并或 rebase 跟进上游，审查控制器与存储契约差异。不要把无关 Git 历史强推到上游，也不要用 checkout 覆盖尚未提交的整合工作目录。

发布前运行 `make test`、`make generate manifests` 并审核生成文件差异，构建两个镜像并记录 digest，在目标 Kubernetes/CSI 组合完成 E2E。保留上游作者与原有声明；本次没有替上游选择或新增许可证。包管理器中的依赖与镜像另遵循其各自许可。
