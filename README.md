# pulsar-operator

用于在 Kubernetes 上运行和运维 Apache Pulsar 与 BookKeeper 的 operator，附带一个 Web 控制台。

- **PulsarCluster**：完整集群（ZooKeeper、BookKeeper、Broker、Proxy）的创建、扩缩容、配置、升级和删除。
- **BookKeeperCluster**：BookKeeper 集群的安全缩容（先复制 ledger，再删除 PVC），以及 journal / ledger 两类 PV/PVC 的磁盘管理。
- **控制台**：页面上的每个操作都直接修改上面两种资源，由控制器负责执行。

完整的功能清单和实现状态见 [docs/operator-scope.md](docs/operator-scope.md)。

## 快速开始

```sh
# 安装 CRD、RBAC 和 operator（镜像见 config/manager/manager.yaml）
kubectl apply -k config/default

# 创建一个示例集群：3 个 ZooKeeper、3 个 bookie、2 个 broker、1 个 proxy
kubectl create namespace pulsar
kubectl apply -f config/samples/pulsarcluster.yaml
kubectl -n pulsar get psc,bkc -w

# 打开控制台
kubectl -n pulsar-operator-system port-forward svc/pulsar-operator-console 8080
# 浏览器访问 http://127.0.0.1:8080
```

控制台默认不需要认证。要开启令牌认证：

```sh
kubectl -n pulsar-operator-system create secret generic pulsar-operator-console --from-literal=token=$(openssl rand -hex 16)
kubectl -n pulsar-operator-system rollout restart deploy/pulsar-operator
# 访问 http://127.0.0.1:8080/?token=<token>
```

## 监控

在集群上开启 `spec.monitoring.enabled` 后，operator 会部署一个 Prometheus 来抓取所有组件的指标。数据存在 PVC 上，默认 20Gi，保留 15 天，并且最多使用磁盘的 80%。控制台里的"Monitoring"区域会显示监控大盘。如果需要长期存储，可以配置 `prometheus.remoteWrite`；如果要接入已有的 Prometheus，配置 `prometheusURL`，并参考 `config/monitoring/podmonitor.yaml`。

## 常用运维操作

都是修改 spec，也可以在控制台里完成：

```sh
# broker 扩容
kubectl -n pulsar patch psc demo --type merge -p '{"spec":{"broker":{"replicas":3}}}'
# bookie 缩容：逐个安全下线，进度见 status.decommission
kubectl -n pulsar patch psc demo --type merge -p '{"spec":{"bookkeeper":{"replicas":3}}}'
# ledger 盘扩容（StorageClass 需支持 allowVolumeExpansion）
kubectl -n pulsar patch psc demo --type merge -p '{"spec":{"bookkeeper":{"storage":{"ledger":{"size":"100Gi"}}}}}'
# 修改 broker 配置，会自动滚动重启
kubectl -n pulsar patch psc demo --type merge -p '{"spec":{"broker":{"config":{"maxMessageSize":"10485760"}}}}'
# 升级，按 ZooKeeper → bookie → broker → proxy 的顺序进行
kubectl -n pulsar patch psc demo --type merge -p '{"spec":{"image":"apachepulsar/pulsar:4.2.5"}}'
```

## 开发

```sh
make generate manifests   # 修改 api/ 之后执行
make test                 # 单元测试 + envtest 控制器测试（会自动下载 kube-apiserver/etcd）
make web                  # 构建控制台
make run                  # 用当前 kubeconfig 在本地运行 operator 和控制台
make image IMG=...        # 构建镜像
```

端到端测试脚本见 [test/e2e/e2e.sh](test/e2e/e2e.sh)，它在真实集群上依次验证消息收发、bookie 扩缩容与数据完整性、下限保护、磁盘变更、broker 扩缩容和配置滚动重启。

## 目录

| 路径 | 内容 |
| --- | --- |
| `api/v1alpha1` | CRD 类型 |
| `internal/resources` | 由 spec 生成 Kubernetes 对象（纯函数） |
| `internal/controller` | PulsarCluster / BookKeeperCluster 控制器 |
| `internal/console` | 控制台 HTTP API |
| `web/` | 控制台前端（React + Vite） |
| `config/` | CRD、RBAC、部署清单和示例 |
