# Pulsar 对接验证结果

> 历史记录：本文记录整合前独立 DiskFleet 模块与上游 Pulsar 源码契约的验证。Pulsar 与存储源码现已纳入同一仓库，后续两节点共享数据盘真实 E2E 已通过；最新结果见[根目录验证记录](../../../docs/validation.md)。以下“尚未运行”和旧环境说明、计数只描述本次历史快照，不代表当前状态。

本次使用用户指定的 [SongOf/pulsar-operator](https://github.com/SongOf/pulsar-operator) 进行源码契约检查与本地兼容验证，观察到的修订为 `3c042c5`。**真实 Pulsar/Kubernetes/LVM/CSI 端到端尚未运行成功。**

## 实际执行结果

| 检查 | 结果 |
|---|---|
| 指定仓库源码读取 | 成功，经远程源码读取工具获取 API、资源生成器、控制器与原始 E2E；本机 git clone/ls-remote 因 DNS 失败 |
| 原实现问题重现 | 本地配置证明单节点类不能容纳三副本硬反亲和；跨节点双 PVC 可无共同候选节点；fake API 重现外部 PVC 被漏掉 |
| 全部 Python 回归 | **101 项通过，0 失败**；其中 Cluster 31 项，Pulsar 集成契约 10 项 |
| CLI 生成清单 | 用明确标为示例的数据成功生成 namespace、三个 fleet StorageClass 和一个真实类型的 PulsarCluster，共 5 对象；未 apply |
| 前端 V8 | JS 语法、7 个页面模板、外部 PVC 只读操作保护、内部 Retain 卷归属显示通过；未进行浏览器点击/CSS 验证 |
| Kubernetes 配置 | DiskFleet Kustomize 渲染成功；Pulsar 对接 JSON 解析与字段/拓扑检查通过；没有 API Server dry-run |
| 验收脚本静态检查 | `bash -n` 与 12 段内嵌 Python 编译通过 |
| 真实脚本入口 | 实际执行后因 `current-context is not set` 退出 **1**，在任何集群变更前停止；这不是 E2E 通过 |
| 上游 Go/envtest | 未运行；没有可用 Go 工具链、依赖下载或集群环境 |

## 修复的内容

1. 增加跨节点的精确 VG 白名单 StorageClass；检查至少三个节点、角色间节点交集、journal/ledger 分盘和容量余量。禁止同名 VG 跨角色误匹配。
2. DiskFleet 能观察该 Operator 创建的 PVC，而不要求补 DiskFleet 管理标签。StorageClass 的池 UID、CSI、Retain/WFFC、厚置备、VG 与拓扑必须匹配。
3. 外部 PVC 在 API/前端均为只读；修改容量走 PulsarCluster，避免两个控制器相互覆盖。即使 Retain 导致 PVC 没有 ownerReferences，仍识别 `app.kubernetes.io/managed-by: pulsar-operator`。
4. 增加针对该 Operator 的真实验收脚本，覆盖实际业务 PVC、Bookie 双卷写读与 Pod 重建、消息持久性、通过父 CR 扩容以及独立 Retain 探针。

源码字段与 PVC 行为见 [类型定义](https://github.com/SongOf/pulsar-operator/blob/3c042c5/api/v1alpha1/common_types.go)、[BookKeeper 资源](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/resources/bookkeeper.go) 和 [BookKeeper 控制器](https://github.com/SongOf/pulsar-operator/blob/3c042c5/internal/controller/bookkeeper_controller.go)。这些源代码依据不等同于实际运行验证。

## 下一步真实运行

按 [集成步骤](../integrations/pulsar/README.md) 在具备 Kubernetes context、三台可调度节点及真实 CSI 挂载权限的测试环境准备资源。PulsarCluster Running 后运行：

```bash
TIMEOUT=600 bash scripts/e2e-pulsar.sh diskfleet-pulsar-e2e diskfleet-pulsar
```

只有脚本全部实际步骤通过后才能给出真实存储链路通过结论。部署使用的 Operator 镜像应从已核查修订构建并记录 digest；尚未验证 bookie decommission、故障盘、节点故障、生产规模与性能。
