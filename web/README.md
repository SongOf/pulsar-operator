# 公用前端

同一 React 外壳提供 Pulsar 和 Storage 页面入口：`/#/pulsar/overview`、`/#/storage/overview`。Pulsar 页面直接访问其 API，`/#/pulsar/audit` 提供操作追溯；Storage 工作区嵌入独立 Storage 页面，二者保留独立凭证和数据请求生命周期。

`src` 是 React 外壳和 Pulsar 页面；`storage/static` 是存储页面；`storage/assets.go` 把存储静态资源提供给 Go API。构建后的 `dist` 随 Pulsar 镜像交付，Storage 页面随独立 API 镜像交付；Gateway 统一 `/`、`/api`、`/storage/` 和 `/storage/api` 的入口。

```bash
web/scripts/dev.sh init
web/scripts/dev.sh unit
web/scripts/dev.sh build
web/scripts/dev.sh run
```

Node 22/npm 用于构建；开发服务器参数可跟在 `run` 后。前端详细结构和服务连通方式见[使用与设计](docs/README.md)，Storage 模板测试通过 Storage 模块 `integration` 执行，Gateway 契约通过系统集成测试执行。
