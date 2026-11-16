# 公用前端设计与开发

`ConsoleShell` 负责模块入口；`PulsarWorkspace` 仅在 Pulsar 页面挂载，`PulsarAudit` 查询审计与关联操作；`StorageWorkspace` 直接渲染存储页面。`storage-views.jsx` 使用共享卡片、表格和状态样式，`StorageDialogs.jsx` 提供计划确认及写操作。模块切换时停止相应轮询并取消未完成的 Storage 请求；两个模块不互传凭证。

路由模块将旧 Pulsar 地址规范化到 `/pulsar/...`，物理节点详情位于 `/#/storage/nodes/<节点名>`。`/storage/` 默认进入 Storage 总览，并保留旧 `#nodes` 等书签。`deployment.js` 只读取服务端声明的 `console-service=storage`、`storage-api-base=api` 元数据，决定独立服务下的同源请求前缀；不会把凭证发送到来自 URL 查询参数或任意元数据的外部地址。Gateway 默认使用 `/storage/api`，Pulsar 使用 `/api`。

Storage 凭证独立保存在当前标签页的 `diskfleet-token`，Pulsar 仍使用 `pulsar-operator-token`。配置探测不携带凭证；业务请求分别鉴权。节点详情显示 Kubernetes Node 和 Agent 的独立状态，启用请求携带节点 UID/resourceVersion；请求接受后仍等待 Agent 上报。磁盘按钮同时检查后端可接管标记和 `validUntil`，页面每 500 毫秒及恢复前台时更新有效期，实际提交前再次核验当前时间。计划确认期间可接收同一设备身份的更新扫描；身份变化需重新规划。外部 Operator 的 PVC 在列表和操作入口均只读。

存储池详情位于 `/#/storage/pools/<池 ID>`，显示自动单盘压测阶段、设备身份、临时测试卷清理、实际测试配置与顺序/随机读写指标，以及人工审核结果。带宽统一显示 MiB/s，延迟显示 ms，未上报指标显示“—”。demo 报告明确标记为模拟数据，不能作为真实性能测量。审核弹窗冻结池 UID/resourceVersion、runID 与 resultDigest；轮询或提交前复核发现变化会阻止提交，要求重新打开审核。拒绝必须填写理由，已拒绝结果可重新审核，通过后不提供撤回。卷申请入口同时要求后端明确允许分配，并且当前成功且已清理的压测结果与 Approved 审核身份一致；仅有 Ready 状态不能分配业务卷。

执行 `web/scripts/dev.sh init` 下载锁定 npm 依赖，`build` 生成 `web/dist`，`run` 启动 Vite。开发代理配置位于 [vite.config.js](../vite.config.js)；完整网关路由位于 [deploy/components/console-gateway](../../deploy/components/console-gateway/nginx.conf)。本地完整业务需另行启动相应 Go API，前端开发服务器不启动后端。

`unit` 执行 `web/src/*.test.js`，包括请求凭证隔离、节点身份前置条件、磁盘过期与身份变化、外部 PVC 只读及真实 JSX 渲染。`web/scripts/check-storage-ui.cjs` 读取 Go API 的 demo 快照，通过 Vite 在不监听端口的模式编译 React 组件并执行服务端渲染；不会扫描真实设备或访问集群。网关契约位于 `test/integration/system/gateway.test.mjs`。这些检查不代替真实浏览器布局、调度器或 CSI 验证。整体测试说明见[测试指南](../../docs/testing/README.md)。
