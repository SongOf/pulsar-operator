# 公用前端设计与开发

`ConsoleShell` 负责模块入口，`PulsarWorkspace` 仅在 Pulsar 页面挂载，集群页面轮询状态，`PulsarAudit` 独立查询审计与关联操作，`StorageWorkspace` 管理嵌入页面。路由模块把旧 Pulsar 地址规范化到 `/pulsar/...`。Storage 页面消息只接受同源、指定窗口和已知协议；模块间不传递凭证。

执行 `web/scripts/dev.sh init` 下载锁定 npm 依赖，`build` 生成 `web/dist`，`run` 启动 Vite。开发代理配置位于 [vite.config.js](../vite.config.js)；完整网关路由位于 [deploy/components/console-gateway](../../deploy/components/console-gateway/nginx.conf)。本地完整业务需另行启动相应 Go API，前端开发服务器不启动后端。

`unit` 执行 `web/src/*.test.js`。存储模板检查位于 `web/scripts/check-storage-ui.cjs`，由 Storage 集成脚本构建 API 并读取模拟快照；网关契约位于 `test/integration/system/gateway.test.mjs`。这两类检查不模拟浏览器布局或完整交互。整体测试说明见[测试指南](../../docs/testing/README.md)。
