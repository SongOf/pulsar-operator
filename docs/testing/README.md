# 测试组织与执行

测试按单元、模块集成、系统集成和真实集群 E2E 分层。前三层通过统一脚本执行，真实 E2E 单独显式运行。

| 层级 | 位置与范围 | 执行入口 |
| --- | --- | --- |
| 单元测试 | `api`、`internal`、`cmd` 中随包的 `*_test.go`；共享前端 `web/src/*.test.js` | `make test-unit`；模块脚本 `unit` |
| Pulsar 集成 | `internal/pulsar/{controller,httpapi}/*_integration_test.go` 与 `test/integration/pulsar` | `make test-integration-pulsar` |
| Storage 集成 | `test/integration/storage`：HTTP→SQLite 持久化工作流、脚本前置检查；`web/scripts/check-storage-ui.cjs` | `make test-integration-storage` |
| Agent 集成 | `internal/agent/controller/*_integration_test.go`：真实 API Server/CRD 校验与并发冲突 | `make test-integration-agent` |
| 系统集成 | Storage→Agent 资源契约、Gateway、部署组合、安装器、CLI/来源完整性、联合 E2E 前置检查 | `make test-system` / `test/integration/system/run.sh` |
| 真实 E2E | `test/e2e/pulsar`、`storage`、`system`；公共实现位于 `test/e2e/lib` | 模块脚本 `e2e` 或根脚本 `e2e-pulsar-storage` / `e2e-pulsar-shared-disk` |

Go 单元测试保留在实现包附近，便于验证私有函数。envtest 文件使用 `//go:build integration`，函数统一以 `TestIntegration` 开头；由于使用包内测试夹具，仍保留在原包中。Storage/Agent 资源契约保留在 `internal/storage/cluster/agent_system_test.go`，使用 `systemintegration` 标签与 `TestSystem` 前缀。无需为了移动测试而公开生产接口。

```bash
scripts/dev.sh init --integration
make test-unit
make test-integration
make test-system
# 或执行前三层全部测试：
make test
```

`make test-offline` 运行所有单元、Storage 集成和系统集成，不启动 envtest，也不连接真实集群；首次使用前仍须准备 Go/npm 依赖缓存。`make test-go` 以两个测试标签执行全部 Go 测试。`make test-pulsar`、`test-storage`、`test-agent` 是各模块单元测试；模块 `dev.sh test` 包含该模块单元与集成测试。旧 `make test-repository` 是系统集成入口的别名。

Pulsar/Agent 集成需要本地 `kube-apiserver`、`etcd`，默认由 setup-envtest 获取。可设置 `KUBEBUILDER_ASSETS` 指向已有目录，路径允许包含空格；缺失或不可执行会失败，不会默默跳过。脚本及测试显式禁用 `USE_EXISTING_CLUSTER`，测试环境不包含 kubelet、scheduler、CSI，因此不证明真实调度或磁盘操作。

Python 仅用于测试和部署辅助，使用标准库；Node 检查使用原页面模板和 Go API 模拟快照。Shell 前置检查用临时 fake kubectl/curl，所有写操作由夹具拒绝。`test/support/fake_timeout.py` 仅为这些假进程提供限时运行，不用于生产 E2E，也不替代 GNU timeout 行为验证。有 kubectl 时仓库测试会实际渲染 Kustomize；无 kubectl 时相应渲染用例明确跳过。

真实 E2E 需要独立测试集群、现有 Ready 池/CSI、正确凭证、GNU timeout 和足够容量，会创建/修改工作负载。Pulsar 生命周期入口是 `modules/pulsar/scripts/dev.sh e2e`，存储卷入口是 `modules/storage/scripts/dev.sh e2e CLASS`；联合流程见[验收手册](pulsar-storage-e2e.md)。Agent 的真实磁盘行为通过 Storage/联合流程验证，没有另设自动初始化盘的 Agent E2E。历史结果见[历史验证记录](../history/validation.md)。

系统目录实验的实时探针位于 `test/e2e/system/system-disk-smoke.py`，由 `scripts/test.sh e2e-system-disk` 显式运行；对应清单位于 `deploy/labs/system-disk`。离线 CLI 样例统一位于 `test/fixtures/pulsar-storage`，不放在部署组合中。根 `scripts/test.sh` 聚合测试层，模块 `scripts/test.sh` 管理本模块用例；`dev.sh` 和 Makefile 均转发到这些实现。
