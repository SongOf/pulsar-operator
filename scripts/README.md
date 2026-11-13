# 脚本组织与使用

根目录脚本负责整个仓库的公共操作和环境组合；模块命令由模块自身维护。`dev.sh` 只转发命令，具体实现按职责拆分。根 Makefile 保留统一命令别名。

| 根脚本 | 职责 |
| --- | --- |
| [init.sh](init.sh) | 初始化共享 Go/npm 依赖，可选 envtest |
| [test.sh](test.sh) | 聚合单元、模块集成、系统集成和显式 E2E |
| [deploy/render.sh](deploy/render.sh) | 选择并渲染部署组合，隐藏磁盘目录布局 |
| [deploy/two-node.sh](deploy/two-node.sh) | 两节点环境预检及管理面安装 |
| [lib/common.sh](lib/common.sh) | 仓库定位、工具检查、Go/envtest、模块镜像与 Kustomize 共用函数；仅供 source |
| [dev.sh](dev.sh) | 兼容的统一分发入口 |

每个后端的 `modules/<模块>/scripts` 使用相同职责划分：

| 模块脚本 | 命令与职责 |
| --- | --- |
| `init.sh [--integration]` | Go 依赖；Pulsar/Agent 支持额外准备 envtest，Storage 无需该选项 |
| `build.sh all` / `image` | 编译二进制或镜像；Storage 还支持 `api`、`plan`、`benchmark` |
| `run.sh run [参数]` | 启动模块；Storage 还支持 `plan` / `benchmark`，Agent 支持 `discover` |
| `test.sh unit` / `integration` / `all` | 本模块测试；Pulsar/Storage 另有显式 `e2e` |
| `deploy.sh render` / `apply` | 渲染或显式安装模块；Storage 另有 `csi-install` |
| `generate.sh objects` | Pulsar/Storage 的 DeepCopy；Pulsar 另支持 `manifests`，Agent 无独立类型生成 |
| `dev.sh <命令>` | 转发原有 `init/build/run/unit/integration/test/image/render/deploy` 等命令 |

公用前端 `web/scripts` 使用 `init.sh`、`build.sh`、`run.sh`、`test.sh`；`check-storage-ui.cjs` 是 Storage 页面专项检查，由 Storage 集成测试调用，不含部署逻辑。

```bash
# 初始化仓库，然后分别运行测试层：
scripts/init.sh --integration
scripts/test.sh unit
scripts/test.sh integration
scripts/test.sh system

# 独立使用模块脚本：
modules/storage/scripts/build.sh all
modules/storage/scripts/run.sh plan --help
modules/agent/scripts/test.sh unit
scripts/deploy/render.sh all > /tmp/all.yaml
```

所有可执行入口支持 `--help`，也可通过绝对路径从其他目录调用。初始化只准备本地依赖，不安装系统软件或集群资源。先准备 `go.mod` 指定的 Go 工具链、Node 22/npm 和 Python 3；镜像构建需要 Docker，部署/渲染需要 kubectl，CSI 安装需要 Helm。离线使用前需预备 Go/npm 缓存与 envtest 二进制。

`scripts/test.sh all` 聚合前三层，`offline` 聚合单元、Storage 集成和系统检查，`go` 执行全部 Go 测试，`shared` 仅运行共享 Go 包单测。系统用例仍由 `test/integration/system/run.sh` 管理；真实验收在 `test/e2e`，通过 `test.sh e2e-pulsar-storage` / `e2e-pulsar-shared-disk` / `e2e-system-disk` 显式运行。

脚本不会推送镜像或自动批准磁盘。`apply`、CSI 安装、两节点安装、完整 E2E 和控制器 `run` 会使用所选集群。Storage `run` 默认本地 demo，cluster 模式才连接 Kubernetes。

`PYTHON`、`KUBECTL` 可指定可执行文件；镜像变量为 `IMG`、`STORAGE_IMG`、`AGENT_IMG`。`CONTROLLER_GEN`、`SETUP_ENVTEST` 可指定可执行文件或空格分隔的命令前缀，不执行 shell 表达式。`ENVTEST_K8S_VERSION` 默认 `1.36.x`，已有 `KUBEBUILDER_ASSETS` 可直接复用；测试始终禁用 `USE_EXISTING_CLUSTER`。

规划输入/输出、Helm values 等用户参数中的相对路径以调用者工作目录为准。含空格的参数直接引用并传给脚本；Makefile 的 `ARGS` 适合简单参数。模块设计和用法见[模块目录](../modules/README.md)，测试边界见[测试指南](../docs/testing/README.md)。
