# 公用前端

同一 React 应用提供 Pulsar 和 Storage 页面：`/#/pulsar/overview`、`/#/storage/overview`。二者使用同一导航和组件，保留独立 API 凭证与请求生命周期。Storage 包含概览、物理节点及详情、磁盘、存储池、PV/PVC、审计和接盘指南。

创建存储池后，在池详情查看自动单盘压测与清理报告，并完成绑定具体结果的人工审核。只有审核通过且服务端确认可分配的池才能申请业务卷；模拟报告会明确标记。

`src` 保存全部 React 页面，构建只输出一份 `dist`，由 Pulsar 和 Storage 镜像共同使用。Gateway 的 `/` 与 `/storage/` 均提供此应用，Storage 请求使用 `/storage/api`；独立 Storage 服务通过页面元数据声明 `/api`，同时支持根路径和 `/storage/`。独立 Storage 服务不提供 Pulsar API，页面会说明这一点并禁用该模块入口。

```bash
web/scripts/dev.sh init
web/scripts/dev.sh unit
web/scripts/dev.sh build
web/scripts/dev.sh run
```

Node 22/npm 用于构建；开发服务器参数可跟在 `run` 后。前端详细结构和服务连通方式见[使用与设计](docs/README.md)，React 行为与渲染检查由 `unit` 执行，Go demo 快照与页面契约由 Storage `integration` 执行，Gateway 契约由系统集成测试执行。
