# Codex 开发规范

适用于整个仓库，文中路径均相对仓库根目录。用户当前任务的明确要求优先于本文件；调整既有架构时同步更新相关规范与文档。这里记录长期约定，不记录某次会话状态、机器路径或临时验证结果。

## 工作方式

- 开始前读取 `git status --short`，识别已有修改；只处理当前任务涉及的内容，不覆盖、回退或提交其他人的改动。
- 先阅读目标模块 README、相关设计和测试说明，再沿调用链确认行为。优先使用 `rg` 查找入口、引用和测试，不凭目录名推断实现。
- 已授权的实现、文档修改和本地验证直接推进；缺少影响实现的关键信息时再澄清，不重复请求已经获得的授权。
- 代码审查先报告有证据的问题，标明文件、位置、触发条件和影响。区分已复现的问题、静态分析结论与待验证风险。
- 完成后用中文说明实际变更、验证结果和未验证项。不把未执行、跳过、依赖缺失或历史测试结果表述为本次通过。

## 模块与代码归属

三个后端共享根 `go.mod`，分别运行、构建镜像和部署。除非任务明确要求，不新增独立 Go module，也不恢复 Python 后端项目。

| 模块 | 入口 | 实现 | Kubernetes 类型 |
| --- | --- | --- | --- |
| Pulsar | `cmd/pulsar` | `internal/pulsar/{controller,resources,httpapi,metrics,audit}` | `api/pulsar/v1alpha1` |
| Storage | `cmd/storage-api`、`cmd/storage-plan`、`cmd/storage-benchmark` | `internal/storage/{httpapi,cluster,store,fleet,model}` | `api/storage/v1alpha1` |
| Agent | `cmd/agent` | `internal/agent/{controller,discovery,lvm,host,model}` | 复用 Storage 类型 |

- `cmd` 负责配置、依赖装配和进程生命周期，业务逻辑放入所属 `internal` 包。
- HTTP 层统一命名为 `httpapi`。Agent 当前没有 HTTP 服务，通过 Kubernetes 资源与 Storage 协作；不要为了目录对齐创建空 API 层。
- `internal/kubeconfig` 是 Storage/Agent 的共享连接配置。Pulsar 使用 controller-runtime 的启动与连接机制，不假定三者自动采用相同 kubeconfig 行为。
- Pulsar 的生命周期逻辑属于控制器，工作负载构造属于 `resources`；Storage 不接管 Pulsar 控制器职责。
- 生产设备发现及 LVM 操作属于 Agent；API、前端和规划器不得绕过 Agent 直接操作宿主磁盘。CSI 负责业务 LV、文件系统、挂载和扩容。
- 公用前端保留在 `web`：`src` 为 React 外壳与 Pulsar 页面，`storage/static` 为 Storage 页面，`storage/assets.go` 提供 Go 嵌入资源。
- Python 标准库测试、部署辅助和实验探针可以保留；不要仅因语言不同删除仍在使用的辅助工具。

## 目录职责

| 目录 | 放置内容 |
| --- | --- |
| `modules/<模块>` | 模块 README、Dockerfile、docs、scripts；不复制 Go 运行代码 |
| `config/<模块>` | 本模块 CRD、RBAC、manager、default 和按需提供的 samples/monitoring |
| `deploy/bundles` | 跨模块组合：all、storage |
| `deploy/components`、`deploy/overlays` | 公用组件、环境覆盖 |
| `deploy/addons`、`deploy/labs` | 外部依赖配置、隔离实验清单 |
| `test/integration`、`test/e2e` | 模块/系统集成、真实集群验收 |
| `test/fixtures` | 跨模块虚构输入和输出示例；包内夹具放对应包的 `testdata` |
| `docs` | 跨模块 design、operations、labs、testing、history、provenance |

根 README 只介绍仓库功能；模块 README 介绍模块设计实现和基本使用。详细模块文档放 `modules/<模块>/docs`；不保留代码搬迁、语言替换等迁移过程记录，当前设计和使用说明直接描述现状。不重新创建 Storage 的 integrations 目录：CLI 样例在 `test/fixtures/pulsar-storage`，上游观察契约在 `docs/provenance`。

移动文件时同步检查相对路径、Go import、嵌入资源、Docker COPY、Kustomize、脚本、CI、测试和 Markdown 链接。纯部署目录重组应对比移动前后的渲染结果，不借搬迁改变资源身份或默认行为。

## 脚本与构建

- 模块 `scripts` 按 `init.sh`、`build.sh`、`run.sh`、`test.sh`、`deploy.sh` 拆分，按需提供 `generate.sh`。`dev.sh` 只分发，不承载业务实现。
- 根 `scripts/init.sh` 和 `scripts/test.sh` 聚合仓库操作，`scripts/deploy` 管理环境部署，`scripts/lib/common.sh` 保存共用函数；Makefile 提供别名，避免复制命令逻辑。
- 公用前端使用 `web/scripts/{init,build,run,test}.sh`。新增可复用命令应落到对应脚本，并更新帮助与文档。
- 脚本自行定位仓库；用户传入的相对输入、输出和 Helm values 路径仍以调用者目录为准。引用变量和参数，保留 `"$@"` 边界，不用 `eval` 解析参数。
- 初始化只准备本地依赖和可选 envtest。不得顺带安装集群资源、批准磁盘或开启写盘开关。
- Dockerfile 放 `modules/<模块>/Dockerfile`，构建 context 为仓库根目录，共用根 `.dockerignore`。不要增加重复的模块 Makefile、依赖目录或构建产物。
- Go 版本以 `go.mod` 为准，前端和 Python 工具版本遵循 CI。依赖变化同步更新 lockfile，并确认兼容性；不为无关修改升级依赖。

常用命令（从仓库根执行）：

```bash
scripts/init.sh --integration                  # 需要时初始化 Go/npm/envtest
modules/pulsar/scripts/test.sh unit
modules/storage/scripts/test.sh integration
modules/agent/scripts/test.sh integration
web/scripts/test.sh
web/scripts/build.sh
scripts/test.sh system
make test                                     # 单元 + 模块集成 + 系统集成
make build
go vet -tags='integration systemintegration' ./...
scripts/deploy/render.sh all
```

完整命令及参数见 [scripts/README.md](scripts/README.md)，不要虚构仓库中不存在的 lint、测试或部署命令。

## 实现与兼容约束

- Go 修改使用 `gofmt`，沿用所在包的命名、错误处理和日志风格。传播 context/取消信号，处理返回错误；只抽取有明确职责或复用需要的接口与公共逻辑。
- Reconcile 保持幂等；spec 表达期望，status 表达观察，避免无变化的状态写入。使用 UID/resourceVersion 和所有权校验防止同名重建或并发更新导致误操作。
- 受管 BookKeeper 的副本、镜像、配置和容量变更写父 PulsarCluster；关系异常时保持只读，不凭同名或单个标签自动接管。
- Storage 对外部 Operator 所有的 PVC 保持只读，所有权限制同时在 API 和 UI 执行。保留 Retain、拓扑校验、释放意图及重试身份约束。
- Agent 的真实写入须同时满足写盘开关与批准状态，并通过设备身份核验、宿主锁和恢复日志；不能跳过系统盘/占用保护，也不能凭 VG 名称认领已有数据。
- 保持已有 API group/version、JSON 字段、资源名称算法、finalizer、SQLite 和设备日志格式兼容。需要破坏性变更时明确迁移与回退方案，补充针对性验证。
- Pulsar/Storage 前端保持独立凭证；同时支持 Storage 根路径和 `/storage/`。iframe 消息校验 origin/source 和协议字段，不传 Token，不把 iframe 加载完成当作 API 就绪。
- Pulsar 用户写操作与控制器关键动作通过审计客户端执行；先持久化意图，完成后记录结果。不要绕过审计客户端，不记录 Token、任意配置值或可能回显秘密的错误正文。普通观察与无变化的调谐不刷审计。
- 管理状态使用各模块独立 PVC，单副本 Recreate；不要将 SQLite WAL 数据放到临时目录或 NFS。
- 区分请求接受、资源创建、PVC Bound、文件系统扩容完成等阶段；demo 行为不得标为真实磁盘能力。

## 生成文件与来源记录

- 修改 API 类型后，通过对应 `generate.sh objects` 更新 DeepCopy，不直接编辑 `zz_generated.deepcopy.go`。
- Pulsar CRD/RBAC 由 `modules/pulsar/scripts/generate.sh manifests` 生成，修改类型/标记后重新生成并审查差异。
- Storage CRD 的校验规则维护在 `config/storage/crd`，需要与 Go 类型一起审查；Pulsar 的生成命令不会更新 Storage CRD。Agent 不定义自己的 CRD。
- 修改导入来源文件时，维护 [upstream-source.json](docs/provenance/upstream-source.json)：移动只调整 `localPath`；内容改变更新对应 `localModifications` 的原因和当前 SHA-256。保留原始路径、来源 URL、导入摘要及固定导入清单。
- 不通过删除来源条目、削弱测试或批量覆盖无关摘要使检查通过。历史证据保留原来的版本、计数和结论，不改写为当前验证结果。

## 按改动选择验证

| 改动 | 验证 |
| --- | --- |
| 模块普通 Go 逻辑 | 对应模块单元测试；涉及实际工作流时加模块集成 |
| Pulsar 控制器或变更前置条件 | Pulsar 单元 + integration |
| Agent 调谐、身份保护、日志与 LVM | Agent 单元 + integration；涉及 Storage 契约时加 system |
| Storage HTTP、持久化或卷流程 | Storage 单元 + integration；跨模块关系变化加 system |
| React / Storage 页面 | 前端 test/build；Storage 模板变更加 Storage integration，Gateway 变更加 system |
| 脚本、清单、目录组织 | Shell 语法、受影响的入口测试、Kustomize 渲染及引用检查；公共入口变化覆盖其调用链 |
| 纯文档 | 检查路径、链接、命令和描述与代码一致，无需重跑无关服务测试 |

包内 Go 单测紧邻源码，不为了搬测试公开生产接口。envtest 用例使用 `integration` 标签和 `TestIntegration` 前缀；Storage/Agent 契约使用 `systemintegration` 标签和 `TestSystem` 前缀。可独立的集成用例放 `test/integration/<模块>`，系统用例放 `test/integration/system`。

`go test ./...` 不包含所有带标签的测试，不能单独作为完整验证。envtest 必须使用隔离 API Server/etcd，显式禁用现有集群；缺少 assets 应报告失败或未执行，不允许静默成功。设备测试使用模拟器和临时文件，真实 E2E 不属于默认 `make test`。

为业务行为变更和错误修复补充有价值的回归测试，优先覆盖并发、身份变更、失败恢复及数据保留；不要添加仅复述实现或为低风险文本修改服务的测试。详细分层见 [测试指南](docs/testing/README.md)。

## 外部操作与交付

- 本地构建、渲染和模拟测试不代表集群部署授权。只有当前任务已经授权对应外部操作时，才运行 apply、安装 CSI、完整真实 E2E、镜像推送或写盘；已有明确授权无需重复确认。
- Token、kubeconfig 凭证、SQLite 用户数据、真实设备日志和临时验收输出不得提交；日志、URL、跨窗口消息和测试失败输出不得泄漏凭证。
- 清理冗余文件前核对引用和职责；包内兼容夹具、跨模块样例、外部来源记录与历史证据不能仅因内容相似就删除。保留未归属当前任务的工作区修改。
- 交付前检查改动范围及 `git diff --check`，运行与修改匹配的验证。只报告实际执行结果及其边界；除非任务要求，不自行提交、推送或部署。

项目背景见 [文档目录](docs/README.md) 与 [模块说明](modules/README.md)。Codex 的文件发现机制见 [官方 AGENTS.md 文档](https://learn.chatgpt.com/docs/agent-configuration/agents-md)；本规范使用根目录单一入口，不修改用户全局 Codex 设置。
