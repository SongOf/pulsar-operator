# Pulsar 使用说明

所有示例从仓库根执行，脚本也支持通过绝对路径从其他目录调用。先安装 Go、Node 22/npm、Python 3；部署还需 kubectl 和可访问的 Kubernetes 集群。

```bash
modules/pulsar/scripts/dev.sh init --integration
web/scripts/dev.sh init
web/scripts/dev.sh build
modules/pulsar/scripts/dev.sh build
modules/pulsar/scripts/dev.sh unit
modules/pulsar/scripts/dev.sh integration
```

`init --integration` 准备独立 envtest 二进制；省略参数只下载 Go 依赖。公用前端单独初始化，两个业务页面共享同一份 npm 依赖。初始化不会创建集群资源。

```bash
# 生成与审查：
modules/pulsar/scripts/dev.sh generate
modules/pulsar/scripts/dev.sh manifests
modules/pulsar/scripts/dev.sh render > /tmp/pulsar.yaml
IMG=YOUR_REGISTRY/pulsar-operator:YOUR_RELEASE modules/pulsar/scripts/dev.sh image
```

构建镜像不会推送镜像或修改部署清单。将审核后的镜像引用写入环境 overlay；按[部署指南](../../../docs/operations/deployment.md)准备 namespace、Console Token Secret 和依赖。`modules/pulsar/scripts/dev.sh deploy` 显式应用 Pulsar 默认配置，额外 kubectl 参数可以传在其后。

在本地连接开发集群时，设置对应 kubeconfig 和 `CONSOLE_TOKEN`，执行 `modules/pulsar/scripts/dev.sh run`。脚本默认用 `web/dist` 提供页面，可通过 `WEB_ROOT` 覆盖；传入的进程参数原样转交 `cmd/pulsar`。此命令实际运行控制器，会调谐该集群中的资源。

PulsarCluster 示例位于 `config/pulsar/samples/pulsarcluster.yaml`，独立 BookKeeper 示例位于 `config/pulsar/samples/bookkeepercluster.yaml`。先调整镜像、容量和 StorageClass，再显式应用 CR。联合存储部署使用根 `scripts/dev.sh render`，参见[Storage 对接](../../storage/docs/pulsar-integration.md)。

PulsarCluster 名称最多 47 字符，独立 BookKeeperCluster 最多 51 字符；名称使用小写字母、数字或连字符，以字母开头，以字母或数字结尾，为生成的 Service 和初始化 Job 保留后缀空间。CRD、HTTP API 和创建页面均执行这一校验；已存在的合法资源不会自动重命名。

部署前准备 `pulsar-operator-system/pulsar-operator-audit` 管理 PVC 所需的基础设施存储，默认申请 1Gi 并使用集群默认 StorageClass。可在环境 overlay 中指定 `storageClassName`，或绑定预建 PV。使用支持 SQLite WAL 的本地文件系统，避免 NFS。Deployment 使用单副本 `Recreate`，更新时控制台和调谐短暂停止，已运行的 Pulsar 工作负载继续运行。

本地审计目录默认 `modules/pulsar/.state`，通过 `AUDIT_DIR` 或 `--audit-dir` 指定；容器中为 `/var/lib/pulsar-operator`。控制台入口为 `#/pulsar/audit`，查询与故障语义见[审计说明](audit.md)。

## 监控存储与恢复

启用内置 Prometheus 前，先检查 `kubectl get storageclass`。未指定 `spec.monitoring.prometheus.storage` 时，监控默认申请 20Gi PVC 并使用集群默认 StorageClass；没有默认类且没有可匹配的静态 PV 时，PVC 和 Prometheus Pod 会一直 Pending。应在父 `PulsarCluster` 的 `spec.monitoring` 中明确指定现有可用类，例如以下配置中的 `EXISTING_STORAGE_CLASS` 必须替换为真实名称：

```yaml
monitoring:
  enabled: true
  prometheus:
    storage:
      storageClassName: EXISTING_STORAGE_CLASS
      size: 20Gi
    resources:
      requests: {cpu: 100m, memory: 256Mi}
      limits: {memory: 1Gi}
```

使用本地存储时，同时检查卷的节点拓扑、空闲容量和节点污点。当前 Prometheus Pod 不继承 Broker 的节点选择器或 tolerations，所选类必须能在其可调度节点上供卷。受管 BookKeeper 的监控使用父 PulsarCluster 配置，不能通过修改子 BookKeeper 来修复。

现有监控 StatefulSet 的存储布局受到保护，直接改变父 CR 中的存储类或容量会被拒绝。若是首次启用失败，先关闭 `spec.monitoring.enabled` 并等待监控 StatefulSet/Pod 删除；PVC 会被保留。仅在确认该监控 PVC 仍为 Pending、`spec.volumeName` 为空且没有任何 PV 的 `claimRef` 指向它时，才能按已核对的 UID 删除这个未绑定声明。然后在父 CR 中设置正确的存储类并重新启用监控，让控制器重建监控资源。已绑定或已有数据的 PVC 必须保留，存储迁移应另行备份和规划；不要删除业务卷或整套 CRD。

恢复后依次检查监控 PVC 为 Bound、Pod Ready、父 CR 的 `status.monitoring.ready=true`，再通过 Prometheus `/api/v1/targets` 和 `up{namespace="目标命名空间",pulsar_cluster="集群名"}` 确认采集成功。控制台接口为 `GET /api/pulsarclusters/{namespace}/{name}/metrics?range=5m`，使用原 Console Bearer Token；应返回 HTTP 200、非空 `pods`/指标序列且没有 `errors`。Ready 只说明进程可服务，不能替代采集验证；刚启动时没有历史样本，速率曲线还需等待多个抓取周期。没有近期写入时，Bookie 延迟指标可能尚未注册或返回 NaN；应在发送测试消息后验证延迟曲线，不能仅凭空闲时缺少该指标判定 exporter 不兼容。
