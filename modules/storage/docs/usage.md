# Storage 使用说明

## 本地初始化与体验

以下命令均从统一仓库根目录执行；`modules/storage` 保存镜像、契约和模块文档，运行代码与构建入口已归入根 Go 项目。

```bash
modules/storage/scripts/dev.sh init
web/scripts/init.sh
web/scripts/build.sh
modules/storage/scripts/dev.sh run
```

在能够监听端口的环境中打开 **http://127.0.0.1:8080**。模拟模式自动生成 24 台节点、144 块磁盘和示例存储池；状态与模拟卷目录保存在 `modules/storage/.state/`，重启后保留。目录模拟不会设置文件系统容量配额，也不会执行真实 `pvcreate`、格式化或挂载。

页面包括：总览、节点、物理盘筛选分页、批量接管计划、存储池、PVC 创建与扩容、保留数据的释放、审计和接盘指南。数据来自同源 API，页面没有把模拟数据写死在 JavaScript 中。

```bash
modules/storage/scripts/dev.sh unit                          # Go HTTP、SQLite、集群与规划测试
modules/storage/scripts/dev.sh build
modules/agent/scripts/dev.sh build             # 构建 Go API、规划、基准及 Agent 工具
modules/storage/scripts/dev.sh integration        # HTTP 工作流与 React 存储契约检查
modules/agent/scripts/dev.sh discover            # 只读发现，仅支持 Linux
modules/storage/scripts/dev.sh benchmark --disks 10000       # 本地元数据负载测试
scripts/dev.sh render storage > /tmp/diskfleet.yaml # API + Agent 配置，不连接集群
```

## 真实集群部署

先在独立 Linux 测试节点验证。节点须具备 LVM2、udev、device-mapper、稳定的 `/dev/disk/by-id`、独占空白测试盘，以及允许特权 DaemonSet 的策略。默认不会纳管任何节点或初始化任何磁盘。

1. 安装独立的 OpenEBS LVM CSI chart（固定版本，不安装其他 OpenEBS 引擎）：

   ```bash
   modules/storage/scripts/dev.sh csi-install
   kubectl -n openebs get pods
   kubectl get csidriver local.csi.openebs.io
   ```

   历史验收组合为 Kubernetes 1.37.2、OpenEBS LVM LocalPV chart/driver 1.10.1、Ubuntu 22.04。验收结果仅适用于记录中的镜像；目标版本须重新验收。两节点测试还使用[固定 values](../../../deploy/addons/openebs-lvm/two-node-values.yaml)，允许 CSI 在控制平面节点运行；部署到其他环境需重新验证。若 kubelet 路径不是 `/var/lib/kubelet`，按驱动 chart 的 `lvmNode.kubeletDir` 配置。

2. 从仓库根目录构建并分发 Storage API 和 Agent 两个镜像：

   ```bash
   STORAGE_IMG=YOUR_REGISTRY/pulsar-operator-storage:YOUR_RELEASE modules/storage/scripts/dev.sh image
   AGENT_IMG=YOUR_REGISTRY/pulsar-operator-agent:YOUR_RELEASE modules/agent/scripts/dev.sh image
   ```

   推送两个镜像到集群可达的 registry，在 `deploy/bundles/storage` 的环境 overlay 中分别设置 `diskfleet` 和 `diskfleet-agent` 的 image。构建变量只设置镜像标签，不改部署清单。后续命令均从仓库根目录执行。

3. 准备独立的管理状态存储，再建立 namespace 和管理凭证并应用部署：

   `diskfleet-system/diskfleet-api-state` PVC 默认申请 1Gi，使用集群默认 StorageClass。该类必须已经能够供卷；不要依赖尚待本 API 接管的新 DiskFleet 池。也可通过环境 overlay 指定 `storageClassName`，或绑定预建 PV。使用支持 SQLite WAL 的本地文件系统，避免 NFS，并为非 root 用户 10001 提供读写权限。部署使用单副本 `Recreate`，更新期间 API 暂时不可用。

   ```bash
   kubectl apply -f config/storage/manager/namespace.yaml
   python3 -c 'import secrets; print(secrets.token_urlsafe(32), end="")' | \
     kubectl -n diskfleet-system create secret generic diskfleet-api-token \
       --from-file=token=/dev/stdin
   kubectl apply -k deploy/bundles/storage
   kubectl -n diskfleet-system rollout status deployment/diskfleet-api
   ```

   已存在的 Secret 应复用，或走自己的凭据轮换流程。API 以非 root 运行，ClusterIP 服务不直接暴露外网。集群模式要求至少 24 字符的 Bearer Token；前端凭证只保存在 sessionStorage。生产接入组织的 TLS/身份代理，限制可管理命名空间。当前 Token 是单一管理员身份，不是多租户 RBAC 系统。

4. 打开节点管理，部署 Agent 并发现磁盘：

   已加入集群的 Node 会出现在节点列表中，不要求它已经有 Agent 或磁盘记录。打开节点详情，点击“部署 Agent”；API 检查节点健康和已安装 DaemonSet 的选择条件后，仅添加启用标签。等待 Agent 显示运行就绪，再查看上报的磁盘列表、扫描时间和有效期。也可以通过命令设置同一标签：

   ```bash
   kubectl label node <实际节点名> storage.diskfleet.io/enabled=true
   kubectl get physicaldisks -o wide
   kubectl -n diskfleet-system logs -l app=diskfleet-agent --tail=100
   kubectl -n diskfleet-system port-forward svc/diskfleet-api 8080:8080
   ```

   打开 http://127.0.0.1:8080 并输入 Secret 中的管理 Token。只选择核实过的空白磁盘，先生成接管计划再提交。计划有效期为 10 分钟，但每次提交还要求最新磁盘扫描仍有效、节点与 Agent 健康；计划有效不等于旧库存可以继续批准。默认扫描有效期为 36 秒，过期时等待下一次成功扫描。提交前及执行前再次核验身份。批量接管按每盘独立计划顺序执行；失败不会回滚已经建立的 VG。

5. 开启真实写入开关。**此命令会允许已提交、approved=true 的磁盘池请求开始初始化磁盘**，必须先检查所有请求：

   ```bash
   kubectl get diskpools -o yaml
   kubectl -n diskfleet-system set env daemonset/diskfleet-agent ENABLE_DISK_WRITES=true
   kubectl get diskpools -w
   ```

   Agent 只接管指定的 by-id/serial/容量组合，不支持自动擦除旧盘。系统盘、挂载盘、swap、分区、RAID 签名、holders、已有外部 PV/VG、缺失身份信息均拒绝。单盘 VG 的身份与恢复日志写入宿主 `/var/lib/diskfleet`，不得删除。该目录及 `/etc/lvm` 应纳入备份。

6. 等待自动单盘压测完成，再在池详情查看报告并审核。报告包含顺序/随机读写吞吐、IOPS、延迟分位数和临时 LV 清理结果。清理未确认、身份不匹配或结果不完整时不能审核；拒绝时填写原因。通过后等待 Agent 发布 StorageClass 并变为 Ready。审批通过后不能撤回或改写，提交前应确认当前设备和业务要求。

   只有显示可创建卷的池才能创建 PVC，然后创建引用该 PVC 的 Pod。可参考 [`config/storage/samples/workload.yaml`](../../../config/storage/samples/workload.yaml)，替换其中 StorageClass；或执行验收脚本：

   ```bash
   bash test/e2e/storage/volumes.sh <实际的-diskfleet-storageclass>
   ```

   脚本创建隔离 namespace，验证真实读写、Pod 重建、扩容和 Retain；不初始化物理盘，不销毁保留的 PV。失败时保留现场。执行前阅读脚本与输出的资源清理说明。

## 依据

- [Kubernetes StorageClass 与 WaitForFirstConsumer](https://kubernetes.io/docs/concepts/storage/storage-classes/)
- [CSI 节点挂载与部署约束](https://kubernetes-csi.github.io/docs/deploying.html)
- [OpenEBS LVM StorageClass 参数](https://openebs.io/docs/user-guides/local-storage-user-guide/local-pv-lvm/configuration/lvm-storageclass-parameters)
- [OpenEBS 官方 Helm 仓库目录](https://openebs.github.io/openebs/)

## 参数与工具

`init` 准备 Go 依赖并检查 Node、Python 3；共享页面另通过 `web/scripts/init.sh`、`web/scripts/build.sh` 准备。`run` 支持 API 启动参数，如 `--mode demo --host 127.0.0.1 --port 8082`。环境变量 `MODE`、`API_HOST`、`API_PORT`、`API_TOKEN`、`STATE_DIR` 继续有效；`WEB_DIR` / `--ui-dir` 指向共享前端构建目录，模块运行脚本默认使用仓库 `web/dist`，镜像使用 `/web`。cluster 模式或非回环监听要求至少 24 字符的 Token。Kubernetes 连接使用 ServiceAccount 或显式 `KUBE_API_URL` 等设置，不自动读取 kubeconfig。

`modules/storage/scripts/dev.sh plan --help` 查看规划参数；`--pools-json` 可携带完整压测/审核证据进行离线规划（不验证实际集群，不能用虚构示例给真实池补签审核），`--output` 不覆盖已有文件，生成结果不自动 apply。输入和输出相对路径以调用者目录为准。`benchmark --disks 10000` 只运行本地元数据基准。

`csi-install` 是显式集群命令：安装独立 OpenEBS LVM LocalPV chart，默认版本 1.10.1，可用 `CSI_CHART_VERSION` 指定经过验证的版本；额外 Helm 参数原样传递，如 `--values deploy/addons/openebs-lvm/two-node-values.yaml`。`init` 不会执行它。镜像和 API/Agent 安装也不会自动批准磁盘。

已有 Ready 池若没有压测报告，会显示 LegacyUnreviewed，禁止新分配；系统不会自动重测旧数据盘或删除已有 StorageClass。已有卷仍可按原所有权规则观察、扩容和 Retain 释放。CSI 不检查 DiskPool 审核，需限制集群中外部或宽泛 `vgpattern` StorageClass 对未审核 VG 的直接访问。模拟模式所有压测值都明确标记为模拟，不代表设备能力。
