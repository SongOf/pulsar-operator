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
