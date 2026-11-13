# 2026-11-13 全仓审查修复

对应原审查的 19 项问题：4 项 P1、12 项 P2、3 项 P3。下表记录实现和验证入口，最终构建、部署和测试结果见[验证记录](validation.md)及[机器可读摘要](verification.json)。原始复现材料保存在 `/root/workspace/pulsar-review-20261113`，修复验证材料保存在 `/root/workspace/pulsar-review-fixes-20261113`。

| 编号 | 问题 | 修复 |
|---|---|---|
| R01 · P1 | Pulsar 接管同名无主 BookKeeperCluster，覆盖独立集群配置 | 既有子资源必须由当前父 CR UID 控制，否则拒绝修改 |
| R02 · P1 | 先停止 Bookie、后写下线状态，状态冲突可能连续停止多个副本 | 下线意图和 PVC UID 先持久化；状态提交失败时不缩副本；清理阶段也先提交再删除 |
| R03 · P1 | StatefulSet 缺失时跳过正在进行的下线流程 | 优先恢复已持久化的 ordinal/阶段；模板重建前额外持久化安全副本数，覆盖不健康集群同时扩卷和缩容的窗口 |
| R04 · P1 | 先删除同名 StatefulSet，再验证所有权 | 删除和更新先检查 controller owner UID；删除同时绑定 UID/resourceVersion，辅助组件同样保护；扩容前也核验 PVC 与工作负载的归属 |
| R05 · P2 | 清空 Bookie StorageClass 绕过不可变校验 | 比较实际模板和 PVC 的类名，包含空字符串变化；拒绝缩容和换类 |
| R06 · P2 | ZooKeeper 存储变更被忽略，却报告 Running | 明确拒绝已有卷大小/类名变化并报告条件 |
| R07 · P2 | Prometheus PVC 未变大，保留阈值却变大 | 拒绝已有卷大小/类名变化，不下发不匹配的保留阈值 |
| R08 · P2 | 同一物理盘/VG 可由不同 DiskPool 重复声明 | 实时检查同节点 VG/路径/序列号冲突，宿主日志绑定池 UID，冲突池不重复计入容量 |
| R09 · P2 | DELETE 成功但响应丢失，Retain 记录丢失 | DELETE 前独立提交释放意图；按原 PVC/PV UID 恢复结果，不把冲突或同名新卷当成原对象 |
| R10 · P2 | Retain 重新绑定缺少 selected-node 时显示错误归属 | 从实际 PV claimRef、CSI VG 和 nodeAffinity 推导，失败则只读并显示核验原因 |
| R11 · P2 | 配置草稿与轮询的新配置混用，覆盖并发修改 | 固定编辑基线和 UID/version；冲突保留草稿，提供重新载入 |
| R12 · P2 | URL Token 出现在网关日志 | 停用 query Token 登录，旧链接跳转到无 Token 路径；日志不记录 query、Referer 或认证头 |
| R13 · P2 | 控制台 GET 后对象被替换，PATCH/DELETE 操作到新对象 | 写 API 必须提供 UID/version；原子 JSON Patch test 或 Delete Preconditions 防止竞态 |
| R14 · P2 | 生命周期 E2E 未鉴权，第一次 PATCH 就失败 | 从 Secret/凭据文件读取 Token，经 stdin 传给 curl；每次写入附身份和版本校验 |
| R15 · P2 | 单池探针不能调度到带控制平面污点的节点 | 核验实际节点拓扑，针对控制平面 NoSchedule 污点添加容忍；异常污点提前拒绝 |
| R16 · P2 | 包含新 Namespace 的 List 做 server dry-run 时失败 | 指南先创建目标 Namespace，再校验整个业务清单 |
| R17 · P3 | 正在删除的同名 PVC 被当成幂等创建成功 | 遇到 deletionTimestamp 返回冲突，要求等待原对象退出 |
| R18 · P3 | 文档仍声称网络不通、未做真实验证或使用旧 CSI | 更新为本次实际版本、验证范围及明确的未验证项目 |
| R19 · P3 | 长池名被哈希为标签后，E2E 按标签查不到 CR | 通过 StorageClass 中的 pool-uid 找到唯一 DiskPool，不用标签值当完整名字 |

验证覆盖实际 Go/envtest API Server/etcd、Python fake Kubernetes/LVM、跨进程文件锁、Node 模型与页面模板、真实 nginx 请求日志、Shell 入口与清单渲染。真实集群上的升级核验保留当前两块数据盘、九个业务 PVC/PV 和 21 GiB ledger；危险竞态通过隔离测试复现，不在运行中的 Bookie 上注入缩容故障。

接口兼容变化：控制台 PATCH、DELETE、restart 必须带 `X-Resource-UID` 和 `X-Resource-Version`，缺少返回 428，身份/版本冲突返回 409。新前端及生命周期脚本已同步。创建接口不受影响。管理凭证改为表单输入，旧 `?token=` 链接不再认证。

存储接口独立使用 `DELETE /api/volumes/{id}?uid=<pvcUid>`：前端提交确认时的 PVC UID，旧 UID 仅恢复旧记录；普通身份失配返回 409。首次无 UID 的旧请求仍兼容，但同名重建且已有释放历史时必须明确新 UID。重复磁盘池的 Conflict 是 API 清单投影，三项容量归零且排除对应可信存储类，不新增 CRD phase 枚举。

升级顺序：先应用新增下线状态字段的 CRD，再升级 Operator/前端；单独更新网关配置和存储 API/Agent。保留 live Agent 的 `ENABLE_DISK_WRITES=true`、磁盘身份日志和已有 CSI 参数。旧 Ready 磁盘日志只有在唯一池、StorageClass 及 LVM 身份全部匹配时补记 poolUID，过程不调用初始化命令。旧版进行中的 Bookie 下线若缺少 PVC UID 则停止并要求人工核对，不能直接清掉状态继续缩容。

仍有限制：两节点共盘实验不证明主机失效高可用；未进行真实浏览器交互、节点/磁盘故障、真实 Bookie decommission 或大规模压测。存储 API 的 SQLite 默认使用 emptyDir，释放意图及审计跨 Pod 重建持久化需要配置独立存储；宿主磁盘所有权日志与这份数据库分开保存。
