# OpenEBS LVM LocalPV 配置

[two-node-values.yaml](two-node-values.yaml) 保存两节点实验使用的 CSI Helm values，包括控制平面节点的调度容忍。该文件不是 Kubernetes 资源，不能直接 kubectl apply。

显式安装入口（会修改所选集群）：

```bash
modules/storage/scripts/deploy.sh csi-install \
  --values deploy/addons/openebs-lvm/two-node-values.yaml
```

脚本默认使用仓库既有的 chart 版本 1.10.1，`CSI_CHART_VERSION` 可以覆盖。普通 `init.sh`、模块部署和 bundles 均不自动安装 CSI。版本、节点 kubelet 路径和镜像需按目标环境核对，见[两节点部署](../../../docs/operations/two-node.md#4-安装或核对-lvm-csi)。
