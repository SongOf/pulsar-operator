# 本机验证报告

> 历史记录：本文记录整合前独立 DiskFleet 模块的本机验证，保留当时的目录、命令和测试计数。当前代码位于 `pulsar-operator/modules/storage/`；后续已经完成两节点真实 LVM/CSI/Pulsar 实验，最新构建、部署与 E2E 结果见[根目录验证记录](../../../docs/validation.md)。以下环境限制仅描述当时状态。

日期：2026-11-13。目录：`/root/workspace/diskfleet`。

## 已执行与通过

| 检查 | 结果与实际覆盖 |
|---|---|
| Python 回归 | 当前 101 项通过，0 失败（初次交付 79 项，Pulsar 对接后增加回归）；实际测试统计见 `verification.json` |
| 磁盘只读发现 | 实际执行 lsblk 与系统状态读取；vda 60 GiB 为系统盘，Protected；vdb 100 GiB 缺少稳定 by-id，同样 Protected |
| 接管后端 | 注入命令执行器验证审批双开关、已有 PV/VG 拒绝、系统盘保护、身份变化、进程中断恢复、所有权校验、幂等及重复纳管；没有执行真实 LVM 写入 |
| 容量并发 | 16 个工作线程同时提交 30 次分配，验证容量不能超配；容量耗尽返回冲突 |
| 文件保留 | 在模拟卷目录写入标记，扩容、释放、重新打开数据库后数据仍保留，Retain 不释放容量 |
| HTTP 协议 | 对实际 BaseHTTPRequestHandler 使用内存传输，覆盖鉴权、计划/提交/PVC生命周期、状态码、非法请求和静态文件；不是 TCP 网络测试 |
| Kubernetes 调谐 | 使用 fake API 验证节点过滤、无变化不更新、不可变身份、SC 漂移拒绝、实际 hostname、Retain、PVC 删除竞态与长节点名 |
| Kubernetes 客户端 | mock HTTPS 请求验证分页 continue、TLS 限制、凭证轮换、错误传播与 merge-patch |
| 部署清单 | `kubectl kustomize deploy` 渲染 12 个对象；YAML 解析、对象唯一性、CRD status 定义与 LVM 宿主锁目录检查通过；不是 API Server schema/CEL 校验 |
| 真实集群脚本 | `bash -n scripts/e2e-k8s.sh` 通过；脚本尚未在集群运行 |
| 前端 | 中文 HTML/JS/CSS 通过真实 HTTP 处理器交付；V8 编译 JS 并执行 7 个页面模板、验证恶意元数据转义及释放中状态操作保护 |
| Python 编译 | `python3 -m compileall -q diskfleet tests scripts` 通过 |

## 元数据负载样本

命令：`python3 scripts/benchmark.py --disks 10000`。

- SQLite 模拟磁盘记录：10,000 条。
- 本次插入耗时：0.114 秒。
- 100 次列表分页查询：中位数 2.208 ms，P95 3.136 ms。

这是当前机器的一次本地元数据样本，不代表磁盘 IOPS、CSI 吞吐、API Server/etcd 或万节点集群的性能保证。

## 当前环境无法完成的验证

- 没有配置 Kubernetes context；没有创建集群或应用任何部署清单。
- 命令沙箱的有效 capabilities 为零，不具备真实挂载/块设备管理权限。没有对 vda/vdb 执行 `pvcreate`、`vgcreate`、格式化、挂载或写入。
- 创建 AF_INET socket 实际返回 `PermissionError: [Errno 1] Operation not permitted`，因此服务未监听 8080，页面不能在本会话预览。
- 本机没有浏览器或 Node。前端通过工具执行环境的 V8 检查，但没有验证浏览器 DOM、CSS 布局、响应式视觉或实际点击交互。`node scripts/check-ui.cjs` 是为具备 Node 的环境提供的复现入口，未在本机以 Node 命令执行。
- 未构建 Docker 镜像，也未安装/运行 OpenEBS CSI。网络 DNS 不可用；项目运行与 Python 测试不依赖下载第三方包。

## 在具备权限的测试环境补齐验收

1. 启动 `python3 -m diskfleet.api`，浏览器打开 http://127.0.0.1:8080，检查七个页面与操作弹窗。
2. 按 README 构建镜像并部署 CRD、Operator、API 和指定版本的 CSI 驱动。
3. 为测试节点提供独占空白物理盘或具有稳定序列号的专用虚拟磁盘；核验设备身份后批准纳管。当前发现器刻意不把 loop 设备当作真实物理盘。
4. DiskPool Ready 后执行 `bash scripts/e2e-k8s.sh <实际StorageClass>`，验证真实 PV 绑定、文件读写、Pod 重建、2→3 GiB 文件系统扩容和 Released/Retain。
5. 扩大部署前测试目标节点数量下的 API Server/etcd、调谐延迟、节点故障、磁盘故障、并发 PVC、备份恢复以及审计持久化。当前版本没有大规模生产验收结论。
