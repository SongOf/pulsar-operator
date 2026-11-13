# Pulsar 操作审计

控制台的 **Pulsar → 操作审计** 页面展示用户请求与控制器执行链路。审计由 Pulsar 独立持久化，Storage 服务不可用时仍可使用。

## 记录范围

| 来源 | 记录内容 |
| --- | --- |
| 用户 API | 鉴权通过后的创建、修改、删除、重启请求，以及参数或 UID/resourceVersion 前置条件拒绝 |
| 资源写入 | CR、配置、Service、StatefulSet、Deployment、Job、PDB、监控资源的创建与变更；PVC 扩容、缩容清理和资源删除 |
| 关键状态 | 集群 phase、Ready/SpecRejected/StorageResized 等条件变化，以及 Bookie decommission 状态 |

普通读取、轮询、仅观察副本数或 observedGeneration 的变化，以及服务端确认没有实际变化的重复调谐，不增加记录。鉴权失败请求不进入业务审计。

每条记录包含操作 ID、记录 ID、时间、来源、操作者标识、资源 kind/namespace/name/UID、版本、变更字段及结果。资源删除后记录仍保留；按 UID 查询可以区分同名重建的资源。用户写入的操作 ID 通过 CR 注解与目标 generation 关联，父 PulsarCluster 到 BookKeeper 子资源继续传递关联。后续 generation 不匹配的外部变更使用系统操作 ID。

控制台使用共享 Token，因此 `console-token:<指纹>` 标识使用的凭证，不代表某个具体个人。控制器使用 `controller/pulsar` 或 `controller/bookkeeper`。未启用 Token 的独立开发实例记为 `anonymous`。客户端地址取连接的 RemoteAddr，经 Gateway 转发时可能是网关地址，不信任任意转发头。

直接通过 kubectl 或其他 Operator 发起的原始请求不经过本 API，只能记录本控制器随后产生的动作。需要识别这类请求的真实用户时，应结合 Kubernetes API Server 审计。

## 执行与结果

先提交 SQLite 审计意图，再执行 Kubernetes 写入，最后记录结果。意图无法落盘时拒绝写入；审计存储不可用会阻止控制器继续修改资源。

- `Started`：已记录意图，操作正在执行或结果尚未完成记录。
- `Succeeded`：HTTP 请求被接受，或对应 Kubernetes 写入已确认；不等于所有 Pod 已就绪。
- `Failed`：请求校验被拒绝，或 Kubernetes 明确拒绝了写入，例如版本冲突。
- `Unknown`：超时、网络故障、进程中断等导致无法确认结果；核对 UID、resourceVersion 和资源现状后再处理。

SQLite 与 Kubernetes 不是同一事务，不能把异常退出推定为失败。重启后未完成的意图标为 `Unknown`。若资源已修改但审计结果保存失败，API 返回 503 并提示先刷新资源，保留原始意图供核对。请求响应通过 `X-Operation-ID` 返回关联 ID。

记录只保留可公开的运维值，例如镜像、副本、容量、存储类和关键状态。任意配置、ConfigMap 数据、环境变量、注解与命令值均脱敏；错误记录不包含可能回显秘密的响应正文。每条变更最多保留 100 个字段，超出时显式标记。

## 查询 API

`GET /api/audit` 与其他 Pulsar API 使用相同 Bearer Token。可组合以下参数：

| 参数 | 含义 |
| --- | --- |
| `operationId` | 查看同一次请求及关联控制器动作 |
| `namespace`、`name` | 精确匹配目标资源 |
| `uid` | 匹配资源 UID 或其所属集群 UID |
| `source` | `api` 或 `controller` |
| `limit` | 每页 1–200 条，默认 50 |
| `cursor` | 使用上一页返回的 `nextCursor` 读取更早记录 |

响应格式为 `{"items":[...],"nextCursor":123}`，按审计序号从新到旧排列；没有下一页时省略 `nextCursor`。插入新操作不会让既有游标重复或漏掉更早的记录。通过统一 Gateway 访问时仍使用 `/api/audit`，Storage 的 `/storage/api/events` 属于另一个模块。

## 状态目录与部署

- 本地默认：`modules/pulsar/.state/audit.sqlite3`；使用 `AUDIT_DIR` 或 `--audit-dir` 指定目录。
- 容器：`/var/lib/pulsar-operator/audit.sqlite3`，由独立 PVC `pulsar-operator-audit` 保存，默认申请 1Gi。
- 单副本 `Recreate` 更新，进程级文件锁防止同一目录同时运行两个审计写入进程。leader election 仍保护控制器，但不能代替审计存储的单实例约束。

管理 PVC 需要预先可用的基础设施 StorageClass 或静态 PV，文件系统须支持 SQLite WAL 和文件锁，避免 NFS。推荐为管理存储使用 Retain 策略并定期备份；数据库没有自动清除记录，应监控容量。

数据库与业务 CR 没有 ownerReference，不随业务集群或 Deployment 删除而自动清理。显式删除整个 Kustomize 组合会包含管理 PVC，需先保存审计并确认存储回收策略。在线备份须使用 SQLite 一致性备份方式；不能只复制主数据库而遗漏 WAL。现有临时目录状态须在替换 Pod 前完成一致性备份与恢复。

## 实现与验证

`internal/pulsar/audit` 负责持久化、脱敏、请求关联和客户端包装；`internal/pulsar/httpapi/audit.go` 负责请求结果与查询；两个 Reconciler 设置控制器上下文。普通单测覆盖写前意图、失败关闭、重启恢复、分页与脱敏；envtest 验证用户请求、真实 API 写入及后续调谐的关联和无变化时不重复记录。
