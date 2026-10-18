# P1 可配置默认代理验收记录

基线：`78feab1`（账号租约 P0 完成后的提交）。只使用假 CLI、静态配置、模拟 HTTP 响应；没有调用真实模型、没有读取或修改真实凭据，没有修改本机 `~/.config/makewand/config.json`，没有安装或下载依赖。

## 行为与文件

- `makewand/proxy.py`、`internal/proxyconfig/proxy.go`：统一默认值、环境覆盖、补齐六个大小写代理变量、NO_PROXY 补充规则与显式 Gemini 绕过规则。存在任一代理变量（即使为空）就不补默认值。显式 Gemini 绕过合并已有两个 NO_PROXY 列表，去重后同时写入两个大小写变量。
- `makewand/config.py`、`internal/config/config.go`：支持 `proxy.default`、`proxy.no_proxy`，默认均为空；环境变量 `MAKEWAND_DEFAULT_PROXY`、`MAKEWAND_DEFAULT_NO_PROXY` 逐字段覆盖，空值禁用对应默认值。
- `makewand/sandbox.py`、`router/cli_sandbox.go`、`router/cli.go`：CLI 沙箱、获授权的宿主启动与 Go CLI 健康检查应用代理。Python 离线沙箱不补代理；不改变父进程环境。
- `makewand/providers/muse.py`：删除 guard 分支固定地址，由公共启动路径解析默认代理。
- `makewand/providers/api_client.py`、`makewand/providers/local.py`、`router/httpclient.go`：API 普通与流式 HTTP 客户端接入代理；使用请求局部策略，避免把默认值写入全局环境。HTTP(S) 可使用 ALL_PROXY 作为协议代理缺失时的回退；NO_PROXY 支持域名、端口、通配符和 CIDR。回环地址也遵循 NO_PROXY，不自动绕过默认代理。
- `makewand/cli.py`、`cmd/makewand/main.go`：Python `status` 文本/JSON、Go `setup` 显示来源，分别为环境变量、配置默认值、无；只输出来源，不输出 URL，所以合法和畸形 URL 的用户名密码都不会出现在状态字段中。
- `README.md`、`CHANGELOG.md`：配置示例、优先级、兼容开关和发布记录。

## 新增与修改测试

- `tests/fixtures/proxy_contract.json`：9 个 Go/Python 共用静态用例，覆盖默认值、大小写、原样保留、显式空值、缺省配置、NO_PROXY 补充边界、环境默认值覆盖与禁用。
- `internal/proxyconfig/proxy_test.go`：共享契约、HTTP(S) 代理、NO_PROXY、Google API 显式绕过、兼容开关优先级、状态不泄露 URL、错误配置拒绝、NO_PROXY 合并幂等及父环境不变。
- `router/proxy_defaults_test.go`：7 类 provider 的假 CLI 实际接收代理、沙箱过滤后的代理保留、普通和流式 API Transport 接入策略。
- `router/cli_test.go`：把 Gemini 无代理时默认绕过的旧断言改为默认不追加 NO_PROXY。
- `tests/test_proxy_defaults.py`：共享契约、7 类 provider 假 CLI、网络/离线沙箱、显式 Gemini 开关、NO_PROXY、API 和 local 路由的模拟响应、实际 status JSON 脱敏、配置类型检查、muse guard 有/无默认配置；40 个测试通过。

## 验证结果

所有测试命令均清除八个大小写代理变量。Go 使用 COMMON.md 指定的 GOCACHE/GOTMPDIR/TMPDIR，设置 GOPROXY=off、GOSUMDB=off，并用临时 HOME 和配置目录隔离宿主状态；Python 使用 `python3 -I -m pytest -q -p no:cacheprovider` 和仓库的测试隔离器。

- `gofmt -l .`：无输出；`git diff --check`：通过。
- `go build ./...`：通过。
- `GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`GOOS=darwin go vet ./...`：通过。
- `go test ./internal/proxyconfig ./internal/config ./cmd/makewand`：通过。
- router 代理相关测试：通过；全包首轮运行有下表 9 组失败，基线 `78feab1` 在相同环境复跑全部复现。最终全包跳过这 9 组既有失败后通过。
- Python 18 个受影响/相关测试文件：257 个测试、158 个子测试通过（包含上述 40 个代理测试）。隔离器报告的被阻断连接来自既有测试对网络隔离器和超时行为的验证，没有访问真实模型端点。

### 基线既有失败

| 测试 | 当前与基线相同的失败原因 |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | 沙箱内假 CLI 无法生成 `oversized-line.ready` 标记。 |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit` | `quiet`、`leader` 子用例等待假 CLI 子进程标记超时。 |
| `TestCustomCLIRefusalIsRetried_R4` | 假 CLI 计数器文件不可写，返回 93，无法进入预期的额度拒绝与重试。 |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | 假 Codex 无法创建调用记录，返回 94。 |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline` | `cancel_after_real_readiness` 子用例无法获得假 CLI 就绪标记。 |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | 假 CLI 计数器不可写，返回 93。 |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | 假 CLI 计数器不可写，返回 93，覆盖了预期的认证错误。 |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | 假 CLI 计数器不可写，返回 93，未进入预期的预算重试阶段。 |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd` | 假 Codex 调用记录不可写，返回 94；多个审查入口子用例因此失败。 |

这些失败涉及当前测试环境中沙箱对临时标记/计数/记录路径的可见性与可写性；本项不修改既有测试夹具或沙箱权限来消除它们。

## 残余限制与 Claude 验收重点

- 本项是离线验收；没有测试真实代理连通性、真实 CLI 自身是否遵循传入变量、真实 muse guard 服务和真实模型行为。代理协议最终仍受各 provider CLI 和 Go/Python HTTP 库支持范围限制。
- Python 和 Go 使用独立实现，共用静态契约降低行为漂移风险，但仍需重点检查已有大小写代理变量、空值、NO_PROXY 保留、双 Gemini 开关优先级和本地 API 的 NO_PROXY 设置。
- Go API 每次请求重新解析配置，不使用 net/http 的进程级环境代理缓存；Python 需要代理时创建请求局部 opener。验收时检查普通和流式入口以及 local 路由均覆盖。
- 状态只显示来源；代理 URL 整体不展示，避免出现用户名密码或畸形 URL 泄漏。
- 合并后再由 Claude 按站主规则设置本机默认代理；本提交没有产品内置地址，也没有设置本机端口。
