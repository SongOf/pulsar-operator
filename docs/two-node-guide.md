# 172.23.254.149 / 172.23.254.150 两节点部署

目标是在已经加入同一 Kubernetes 集群的两台节点上部署 Pulsar Operator、磁盘 Agent/API、统一控制台、OpenEBS LVM CSI 和 Pulsar。保留现有 Kubernetes、CNI、containerd、节点名与控制平面 taint，不执行 `kubeadm init/reset`。

**2026-11-13（北京时间）已完成两节点真实部署和共享数据盘 E2E。** 节点为 `k8s-control-1`（149）与 `iz6weeh0nfgmlsr99i8atmz`（150）；管理面、两节点 Agent、OpenEBS LVM CSI 和 `diskfleet-pulsar-data-lab/pulsar-lab` 已运行。3 ZooKeeper、3 Bookie、2 Broker、1 Proxy、1 AutoRecovery 就绪，9 个 PVC Bound，三个 ledger 均已扩至 **21Gi**。当前采用每节点一块 100Gi 数据盘的 `two-node-shared-disk-lab`，精确设备与结果见[共享数据盘指南](shared-data-disk-lab.md)。

控制台入口为 `http://172.23.254.149:30080` 或 `http://172.23.254.150:30080`，需客户端可达该私网。第 2–6 节保留首次部署流程与独立 journal/ledger 双盘方案；现有部署无需重跑初始化或 `install`，升级按第 8 节执行。此前执行环境的网络限制已解除，旧的“尚未安装”状态不再代表本次交付。

## 布局和资源

| 模块 | 两节点配置 |
|---|---|
| Kubernetes | 复用已有集群；新增 worker 不会自动形成双控制平面或 etcd 高可用 |
| Pulsar Operator / 存储 API / 统一网关 | 各 1 个 Deployment 副本，可在指定两节点调度 |
| 磁盘 Agent / CSI node plugin | 每个选定节点 1 个 Pod，允许控制平面 `NoSchedule` taint |
| ZooKeeper / Bookie | 各 3 副本，Preferred 反亲和，不能保证 2+1 分布 |
| Broker / Proxy / AutoRecovery | 分别 2 / 1 / 1 副本；Broker 的 E/W/A quorum 为 2/2/2 |
| 当前本地卷 | 每节点一块 100Gi 数据盘、一 VG；三个角色共用池，CSI 分配独立 LV/PV/PVC |
| 独立双盘方案 | `two-node-lab` 仍要求每节点 journal/ZooKeeper 与 ledger 分盘；当前共享配置不改变其要求 |

业务组件稳态 requests 合计 **20.5 GiB 内存、3.3 CPU**，还未包含 Kubernetes、CSI、管理服务和初始化 Job。检查现有 Pod 的资源占用，以及单节点能否容纳最大 3Gi request 的 Pod；总量足够不代表调度必然成功。这个 profile 不按高可用生产容量设计。

**当前共享 profile：** 两节点分别使用 `lab-data-149` / `df_lab_data_149`、`lab-data-150` / `df_lab_data_150`。初始计划按每池至少 94Gi 空闲检查全部副本可能集中到一台的情况；业务初始 90Gi，验收后为 93Gi，另保留 1Gi 探针。系统盘不参与这些 VG。

**独立双盘 profile：** 每节点至少两块已核实的数据盘，共用的 ZooKeeper/journal 池至少 **30Gi 可用**，独立 ledger 池至少 **64Gi 可用**。若 ZooKeeper 独立用盘，则其池与 journal 池各至少 15Gi。Preferred 不保证 2+1 分布，容量按每个池能承载全部 3 副本校验；这里不假定系统盘或 `/dev/vdb` 是可擦写空盘。

任一节点故障都可能造成 ZooKeeper 失去多数派或 Bookie 无法满足 quorum；本地 PV 不会自动复制到另一台机器。两种两节点 profile 都是功能联调配置，共享 profile 额外没有 journal/ledger 物理 I/O 隔离。原三节点存储 E2E 仍要求三节点；当前通过的是独立共享盘 E2E。

## 1. 读取真实集群状态

```bash
cd /root/workspace/pulsar-operator
export KUBECONFIG=/etc/kubernetes/admin.conf
bash scripts/deploy-two-node.sh preflight
```

入口默认就是 `preflight`，只读查询：按 InternalIP 查找 `.149` 和 `.150` 的实际 Node 名，检查 Ready/cordon、taint、Allocatable、现有 CSI 与节点注册情况。再对输出的两个实际 Node 名执行 `kubectl describe node NODE_NAME`，查看 Allocated resources 与已有 Pod requests；脚本没有自动证明资源余量足够。指定其他 kubeconfig 可加 `--kubeconfig /path/to/config`。不要把 IP 当作 Node 名；也不要为调度方便删除整个集群的 control-plane taint。

## 2. 构建并分发整合版镜像

在有 Docker/buildx、能够下载构建依赖的构建机，使用**本次整合仓库**执行。`REGISTRY` 改为两个节点都能拉取的实际仓库；下面的值是占位示例。

```bash
export REGISTRY=YOUR_REGISTRY/YOUR_PROJECT
export RELEASE=two-node-20261113-v1
make images IMG="$REGISTRY/pulsar-operator:$RELEASE" STORAGE_IMG="$REGISTRY/pulsar-operator-storage:$RELEASE"
docker push "$REGISTRY/pulsar-operator:$RELEASE"
docker push "$REGISTRY/pulsar-operator-storage:$RELEASE"
```

Pulsar 镜像同时包含 Go Operator 和构建后的 React 页面；存储镜像包含 Agent、API 和存储前端。仅使用上游旧镜像不会包含这次集成。部署引用应记录准确 tag 或 digest。需要鉴权的仓库应先为两个命名空间配置对应 imagePullSecret；不要在仓库提交凭证。

离线方式可在构建机 `docker save` 两份镜像后传到两节点，分别执行 `ctr -n k8s.io images import /path/to/images.tar`；清单 image 名称必须与导入名称一致。还需提前提供 Pulsar、nginx 和 CSI 的依赖镜像。仅导入控制平面一台会使另一个节点 `ImagePullBackOff`。本次实际使用独立 BuildKit 构建 OCI 镜像，并导入两节点现有 containerd 的 `k8s.io` namespace；没有安装 Docker 或覆盖 Kubernetes 使用的 containerd。镜像版本和最终验证见[验证报告](validation.md)。

## 3. 安装管理面与磁盘发现

以下是**首次安装**命令。在控制平面终端设置已分发的实际镜像引用：

```bash
export PULSAR_IMAGE=YOUR_REGISTRY/YOUR_PROJECT/pulsar-operator:two-node-20261113-v1
export STORAGE_IMAGE=YOUR_REGISTRY/YOUR_PROJECT/pulsar-operator-storage:two-node-20261113-v1
bash scripts/deploy-two-node.sh install --timeout 600s
```

安装入口渲染 `deploy/two-node`、核对镜像和禁写配置，再创建或复用两个管理 Token Secret、给核实的节点加标签、安装 4 个 CRD、两个 Operator/Agent、API 和统一网关，并等待 rollout。Token 随机生成且不会输出到安装日志。节点标签分别为 `pulsar.songof.dev/two-node=true` 与 `storage.diskfleet.io/enabled=true`。只有前者限定所有组件的范围，后者额外控制磁盘 Agent。

此安装入口只完成管理面和只读发现，不创建 DiskPool、批准磁盘、开启写盘、安装 CSI 或创建 PulsarCluster。**本次这些后续步骤已完成，Agent 现为 `ENABLE_DISK_WRITES=true`。** 再次运行 `install` 会因现有 Agent 已启用写入而被拒绝，这是防止初始禁写模板覆盖运行配置；应按第 8 节升级。

## 4. 安装或核对 LVM CSI

两节点都需 LVM2 和可用的 `dm-snapshot`。在各节点普通终端核实 `lvm version`、`lsmod`、`/var/lib/kubelet` 与 containerd 配置；缺少依赖时按节点系统安装，例如 Ubuntu 使用 `apt-get install -y lvm2`，并 `modprobe dm-snapshot`。这些命令不创建 VG，也不格式化磁盘。

本次已安装官方 chart / driver **1.10.1**，release 为 `openebs-lvm`，namespace 为 `openebs`，两个 node plugin 和 controller Ready，两节点均已注册 `local.csi.openebs.io`。值文件为 [lvm-values.yaml](../deploy/two-node/lvm-values.yaml)，针对两个节点添加选择器和 taint 容忍。真实 PVC 创建、挂载和扩容已由共享盘 E2E 检查；这只说明本次环境与所测路径通过，不覆盖所有 Kubernetes/CSI 功能。安装版本来源记录在 `openebs-lvm-validation.json`，官方参考为 [Helm 索引](https://openebs.github.io/lvm-localpv/index.yaml)与 [1.10.1 参数源码](https://github.com/openebs/lvm-localpv/blob/v1.10.1/deploy/helm/charts/values.yaml)。

先执行 `helm list -A`、`kubectl get csidriver,csinode`。已有 `local.csi.openebs.io` 时复用并核对该安装的版本、namespace 与 selector，不创建第二套 driver。只有首次安装时执行：

```bash
helm repo add openebs-lvm https://openebs.github.io/lvm-localpv
helm repo update openebs-lvm
helm template openebs-lvm openebs-lvm/lvm-localpv --version 1.10.1 \
  --namespace openebs -f deploy/two-node/lvm-values.yaml > /tmp/pulsar-lvm-reviewed.yaml
# 先审查渲染结果和已有快照组件归属，再安装同一版本。
helm install openebs-lvm openebs-lvm/lvm-localpv --version 1.10.1 \
  --namespace openebs --create-namespace -f deploy/two-node/lvm-values.yaml \
  --wait --timeout 10m
kubectl -n openebs get deploy,ds,pod -o wide
kubectl get csinode -o yaml
```

已有全局 snapshot-controller 时按其管理方式设置 `lvmController.snapshotController.enabled=false`；快照 CRD 已归属其他安装时设置 `crds.csi.volumeSnapshots.enabled=false`，不强抢 Helm ownership。参考[官方安装要求](https://github.com/openebs/lvm-localpv/blob/v1.10.1/README.md)。本次使用 Helm 3.22.1 完成安装，已配置两节点启动时加载 `dm-snapshot`。

必须看到两个目标节点都注册 `local.csi.openebs.io` 且 node plugin Ready、controller rollout 完成；只有 CSIDriver 对象存在不足以证明可用。已有 CSI 服务其他节点时，不直接应用本实验的两节点 selector 缩小其范围。

## 5. 纳管已核实的数据盘

```bash
kubectl get physicaldisks,diskpools -o wide
kubectl -n diskfleet-system get pods -o wide
```

按[物理盘手册](../modules/storage/docs/physical-disks.md)核对实际节点、`/dev/disk/by-id`、serial、精确字节容量与用途。初始 Agent 保持 `ENABLE_DISK_WRITES=false`。只对已经确认可初始化的独立数据盘建立计划，审查所有已批准的待执行池后，才开启写盘并逐一批准。此文不提供猜测设备名的自动格式化命令。

选择独立双盘 `two-node-lab` 时，每节点按 journal/ZooKeeper 和 ledger 建两个独立池。**本次已经授权并初始化的是共享数据盘方案**，每节点只有一个 Ready 池，使用 `two-node-shared-disk-lab`。不要为满足双盘示例把同一设备重复建池。原始盘不需要手工挂到 `/mnt` 或写 fstab：Agent 创建 LVM PV/VG，CSI 为业务 PVC 创建 LV、文件系统并通过 kubelet 挂进 Pod。`/var/lib/diskfleet` 必须保留。

## 6. 从真实 Ready 池生成两节点 Pulsar

当前共享数据盘集群已经部署完成，生成方式见[共享数据盘指南](shared-data-disk-lab.md#从真实-ready-池生成清单)。以下保留的是**新建独立双盘环境**的 `two-node-lab` 示例，不能直接用于当前两个共享池。初始生成器输出 ledger=20Gi，不应回写现有已扩到 21Gi 的业务集群。

本地生成器通过 Kubernetes REST 读取池。先另开一个控制平面终端启动只监听 loopback 的代理：

```bash
KUBECONFIG=/etc/kubernetes/admin.conf kubectl proxy --address=127.0.0.1 --port=8001
```

在原终端将池名替换为实际值。三个等级必须覆盖已经打两节点标签的同两台机器；journal 与 ledger 不可共盘。不要传入仓库 `*.example.json`，其 UID 是虚构测试数据，也不要把原始 `kubectl get diskpools -o json` 当成生成器的 normalized `--pools-json`。

```bash
KUBE_API_URL=http://127.0.0.1:8001 python3 scripts/prepare-pulsar.py \
  --profile two-node-lab \
  --zookeeper-pools ACTUAL_JOURNAL_POOL_149,ACTUAL_JOURNAL_POOL_150 \
  --journal-pools ACTUAL_JOURNAL_POOL_149,ACTUAL_JOURNAL_POOL_150 \
  --ledger-pools ACTUAL_LEDGER_POOL_149,ACTUAL_LEDGER_POOL_150 \
  --namespace diskfleet-pulsar-lab --name pulsar-lab \
  --output /tmp/pulsar-two-node-plan.json
```

生成器只读检查 Ready 状态、池 UID、VG 白名单、真实 hostname、相同节点交集与可用容量，生成 namespace、3 个 fleet StorageClass 和 PulsarCluster；不执行 apply。两节点角色配置保留默认 quorum，全部组件及 metadata Job 都限制到标签选定的节点。输出已存在会拒绝覆盖。

审核真实 UID/VG/hostname 和镜像可达性后执行：

```bash
kubectl create namespace diskfleet-pulsar-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --dry-run=server -f /tmp/pulsar-two-node-plan.json
kubectl apply -f /tmp/pulsar-two-node-plan.json
kubectl -n diskfleet-pulsar-lab get psc,bkc,pod,pvc -o wide
kubectl -n diskfleet-pulsar-lab wait pulsarcluster/pulsar-lab \
  --for=jsonpath='{.status.phase}'=Running --timeout=20m
kubectl -n diskfleet-pulsar-lab get pulsarcluster pulsar-lab \
  -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
```

第一条会创建专用 namespace；这是为了让后续 server dry-run 能验证其中的资源，dry-run 中的 Namespace 本身不会实际建立。Running 后还需确认两个 generation 相同，3 个 ZooKeeper PVC、3 个 journal PVC、3 个 ledger PVC 全部 Bound，实际 PV 的 CSI driver/Retain/节点拓扑与 Pod 匹配。等待中的 `Pending` 要检查 Events，区分容量、taint、节点选择、镜像拉取和 CSI 错误。不要通过降低副本/删除 PVC 掩盖问题。

## 7. 控制台与功能验证

当前私网入口为 **`http://172.23.254.149:30080`** 与 **`http://172.23.254.150:30080`**，对应 Service `pulsar-operator-system/console-gateway-lab`。两个地址均已实际验证 HTTP/静态资源和鉴权 API；浏览器 DOM、布局和点击流程未纳入本次自动验证。也可使用本地端口转发：

```bash
kubectl -n pulsar-operator-system port-forward svc/console-gateway 8080:8080
```

浏览器访问该终端所在机器的 `http://127.0.0.1:8080`。如果在远端服务器执行，应从自己的电脑建立 SSH 本地端口转发再访问。Pulsar 和存储使用两个独立管理 Token；只有登录时在自己的终端分别读取，不把值贴到公共日志：

```bash
kubectl --kubeconfig=/etc/kubernetes/admin.conf -n pulsar-operator-system get secret pulsar-operator-console -o jsonpath='{.data.token}' | base64 -d
kubectl --kubeconfig=/etc/kubernetes/admin.conf -n diskfleet-system get secret diskfleet-api-token -o jsonpath='{.data.token}' | base64 -d
```

Pulsar API 已核对真实父 Pulsar→BookKeeper UID 关联；存储 API 可见两个 Ready 池和 9 个只读业务 PVC。业务 PVC 扩容应通过 PulsarCluster 声明执行。需要人工核对页面时，检查 Broker、Bookie 的独立管理区域及物理存储页。可另建一个专用消息探针，下面使用当前实际 namespace：

```bash
RUN_ID=$(date +%s)-$RANDOM
TOPIC="persistent://public/default/two-node-smoke-$RUN_ID"
SUB="two-node-smoke-$RUN_ID"
kubectl -n diskfleet-pulsar-data-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-admin topics create-subscription "$TOPIC" -s "$SUB" -m earliest
kubectl -n diskfleet-pulsar-data-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-client --url pulsar://localhost:6650 produce "$TOPIC" -m "two-node-$RUN_ID" -n 20
timeout 120s kubectl -n diskfleet-pulsar-data-lab exec pulsar-lab-broker-0 -- \
  bin/pulsar-client --url pulsar://localhost:6650 consume "$TOPIC" -s "$SUB" -n 20
```

必须观察 20 条发送成功、20 条接收成功且消息内容匹配。上述简易消息探针会留下 topic/subscription，本身不执行重建、扩容或 Retain。本次**完整共享盘验收已经通过**，由独立入口执行：

```bash
# 仅重新检查标签、节点、池、业务 PVC/PV 和挂载拓扑，不写入业务数据。
PREFLIGHT_ONLY=1 TIMEOUT=600 bash test/e2e/pulsar-shared-disk.sh \
  diskfleet-pulsar-data-lab pulsar-lab
```

首次完整验收记录在 `/root/workspace/pulsar-deployment-evidence/shared-e2e.log` 和 `diskfleet-pulsar-e2e.WnikXAer/`：20 条消息跨 bookie-0 重建保留、journal/ledger 文件以 UID 10000 写读、三个 ledger 20→21Gi、全部 9 组 PVC/PV UID 保持、独立 1Gi Retain PV Released。去掉 `PREFLIGHT_ONLY=1` 会再次增加三个 ledger 各 1Gi，并额外保留一个 1Gi PV；执行前核算剩余空间。

另可对明确选定的真实存储类运行 `make e2e-storage STORAGE_CLASS=...`，该流程另有保留卷预算，执行前查看[存储验收手册](../modules/storage/docs/physical-disks.md#5-真实-kubernetes-验收)。原 `make e2e-pulsar-storage` 保持三节点限定，不适用于共享 profile。两节点验收不证明主机/磁盘故障高可用、大规模管理或生产性能。

节点、镜像、物理盘发现修复后的最终状态以及完整检查范围见[验证报告](validation.md)。

## 8. 现有部署的镜像升级

当前 Agent 已设置 `ENABLE_DISK_WRITES=true`。`scripts/deploy-two-node.sh preflight` 仍可只读运行；`install` 会主动拒绝覆盖现有 Agent。不要为了绕过该保护切换回 `false` 或重新 apply 初始 `deploy/two-node`：这可能覆盖运行中的镜像和环境配置。

构建并向**两节点**分发新的镜像 tag/digest，确认 API/Agent 版本兼容后，分别更新现有工作负载的镜像。以下命令只更改指定容器 image，保留 Secrets、写盘开关、节点选择、卷挂载及 CR 状态：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
# 先设置已审核且两节点都可用的新镜像引用；使用新 tag 或固定 digest。
export PULSAR_IMAGE=YOUR_REGISTRY/YOUR_PROJECT/pulsar-operator:YOUR_NEW_RELEASE
export STORAGE_IMAGE=YOUR_REGISTRY/YOUR_PROJECT/pulsar-operator-storage:YOUR_NEW_RELEASE
kubectl -n pulsar-operator-system set image deployment/pulsar-operator operator="$PULSAR_IMAGE"
kubectl -n pulsar-operator-system rollout status deployment/pulsar-operator --timeout=600s
kubectl -n diskfleet-system set image deployment/diskfleet-api api="$STORAGE_IMAGE"
kubectl -n diskfleet-system rollout status deployment/diskfleet-api --timeout=600s
kubectl -n diskfleet-system set image daemonset/diskfleet-agent agent="$STORAGE_IMAGE"
kubectl -n diskfleet-system rollout status daemonset/diskfleet-agent --timeout=600s
kubectl -n diskfleet-system get daemonset diskfleet-agent \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].env[?(@.name=="ENABLE_DISK_WRITES")].value}{"\n"}'
kubectl get diskpool lab-data-149 lab-data-150
kubectl -n diskfleet-pulsar-data-lab get psc,bkc,pod,pvc
```

最后的开关应仍为 `true`，两节点 Agent Ready、两个池 Ready、业务 PVC 保持 Bound。升级不删除 `/var/lib/diskfleet` 的设备所有权日志，不重建 VG/PV/PVC，不覆盖已扩容的 ledger 声明，也不重建管理 Token。若发布包含 CRD 或配置变更，单独审查对应增量；`set image` 不会自动更新 CRD。
