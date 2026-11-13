# 部署组合与环境配置

`config/<模块>` 保存各模块的 CRD、RBAC、工作负载及示例。此目录负责跨模块组合、公共组件和环境差异，不复制模块配置。

| 目录 | 职责 | 是否自动包含 |
| --- | --- | --- |
| [bundles/all](bundles/all/kustomization.yaml) | Pulsar + Storage + Agent + Console Gateway | 完整部署组合 |
| [bundles/storage](bundles/storage/kustomization.yaml) | Storage + Agent，独立于 Pulsar | 存储部署组合 |
| [components/console-gateway](components/console-gateway/kustomization.yaml) | 同源路由、Gateway Deployment/Service 和 nginx 配置 | 由 all 引用；namespace 由 Pulsar 基础配置提供 |
| [overlays/two-node](overlays/two-node/kustomization.yaml) | 两节点选择器和控制平面污点容忍，引用 all | 仅选择两节点环境时使用 |
| [addons/openebs-lvm](addons/openebs-lvm/README.md) | 外部 CSI 的 Helm values | 显式安装，不随 bundles 部署 |
| [labs/system-disk](labs/system-disk/README.md) | 系统盘目录实验的 provisioner 与 Pulsar CR | 独立实验，默认组合不引用 |

渲染入口：

```bash
scripts/deploy/render.sh all > /tmp/all.yaml
scripts/deploy/render.sh storage > /tmp/storage.yaml
scripts/deploy/render.sh two-node > /tmp/two-node.yaml
# 等价兼容入口：make render / make render-two-node
```

渲染不会连接集群。审核镜像与凭证配置后，按[部署指南](../docs/operations/deployment.md)显式安装。两节点预检与安装使用 `scripts/deploy/two-node.sh`，规则见[两节点指南](../docs/operations/two-node.md)。

`overlays/two-node/console-nodeport.yaml` 是单独应用的可选 Service，不包含在该 overlay 中。CSI values、实验 CR 和测试数据也不会被默认组合引入；不要递归 apply 整个 deploy 目录。真实验收脚本属于 `test/e2e`，离线虚构数据属于 `test/fixtures`。
