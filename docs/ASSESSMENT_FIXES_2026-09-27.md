# 独立评估修复记录 · 2026-09-27

本记录对应 [基线评估](MAKEWAND_INDEPENDENT_ASSESSMENT_2026-09-27.md) 的 R01–R14。修复及回归测试纳入后续整合提交；原评估保留用于追踪修复前的行为，不代表修复后的代码状态。

| 编号 | 修复后的行为 | 主要回归证据 |
|---|---|---|
| R01 | 用户可见性同时受用户、组织和项目范围限制；租户权限不能修改全局账号属性或兑换全局管理员权限 | `serveradmin/authorization_regression_test.go` |
| R02 | 请求逐次校验用户和成员授权状态；密码、角色、成员变更使对应旧凭据失效；浏览器 cookie 绑定持久化账号状态；成员操作只撤销目标租户凭据 | `router/user_authorizer_test.go`、`serverauth/multi_manager_test.go`、服务端授权回归 |
| R03 | Go 在执行前封存内容和权限，TUI 应用同一结构化产物；Python 绑定测试、复审、候选增删改计划和交付提交，阻止测试改源、复审改源、Git 索引篡改及提交阶段混入未审查内容 | `internal/engine/verification_regression_test.go`、`internal/tui/acceptance_regression_test.go`、`tests/test_assessment_regressions.py`、`tests/test_delivery_binding.py` |
| R04 | 测试计划取自基线，新增 npm 配置不能替换 Go 验收；基线 Go 测试逐项核对；候选进程的测试输出最高仅获 Strength 1，不能授权自动应用 | Go 验证回归及 TUI 接受门禁回归 |
| R05 | race 只接受明确、有效的结构化裁判通过；双方被拒、协议缺失或格式错误均不选择最快候选 | Python race 通过、拒绝、无效协议与候选应用回归 |
| R06 | 启动器采用 Python `-I` 和明确的安装根路径；升级原子替换链接本身，避免写坏链接目标 | `scripts/test_installation.py`、`cmd/makewand/python_engine_test.go` |
| R07 | 发行归档同时包含 Go 可执行文件和 Python 引擎，版本保持一致；Homebrew、Scoop 及解包后入口使用完整布局 | 删除源码副本后从干净归档执行版本与主命令帮助检查 |
| R08 | Go/Python 应用及回滚保留 Unix 权限；Python 复制中再次核对字节与模式，拒绝篡改；回滚失败会明确报告保留的备份 | 可执行文件应用、权限篡改、写入失败回滚回归 |
| R09 | Go 测试/构建使用独立网络命名空间；依赖安装仍可联网 | 真实 Bubblewrap 检查命令参数、隔离状态和宿主回环访问 |
| R10 | `npm test` 不再强加 Jest 参数；Python 检测依据实际测试文件/配置；混合项目不会因存在 `tests/` 而漏掉根目录 Python 测试 | 真实 Node 测试与混合 Python/Node 临时项目 |
| R11 | 相对 quota 重置时间只转换一次为绝对期限；重复读取不重复扣减经过时间 | 小数和复合时长的重复处理回归 |
| R12 | 官网写入运行时使用的配置；local 默认关闭；兼容旧安装字段；Go 保存共享配置保留 Python 字段并防止恢复已删除的 Go 密钥 | Python 默认/显式/旧配置回归、Go 共享配置 roundtrip 回归 |
| R13 | 默认 `subscription_only`；发现 API key 不会自动允许 Makewand 云 API 或付费回退；显式 `allow_paid` 才启用；状态/setup 展示策略 | Go 路由与配置策略测试、Python HTTP 调用前阻断回归 |
| R14 | 官网示例使用实际参数；删除无打包元数据的 editable-install 指引；serve 示例保留 TLS 要求 | 22 条官网 CLI 示例 smoke |

同步处理了评估中的配套问题：统一 Go/Python/dispatch/安装/离线 benchmark 门禁；单 provider 仍执行单独复审，复审错误或未解决缺陷阻止接受；新增可复验的固定任务 benchmark 执行器，并删除没有数据支持的多模型优势宣称。

最终检查额外发现旧工具链存在已知标准库漏洞。最低 Go 版本已升至 `1.26.7`，CI 与发布通过 `go.mod` 使用同一版本。官方记录包括 [os.Root 符号链接边界问题](https://pkg.go.dev/vuln/GO-2026-4970) 和 [URL 路径解析复杂度问题](https://pkg.go.dev/vuln/GO-2026-6218)。工具链下载核对了 [官方发布校验和](https://go.dev/dl/)。

## 验证

执行环境为 Linux。检查使用临时配置、SQLite、仓库、固定测试程序和模型响应桩；没有发起真实模型调用。

- Go 1.26.7 全量测试：833 个顶层测试、含子测试共 1,034 个 pass 事件，无跳过；真实 Bubblewrap 隔离测试执行成功。
- 全量竞态检测、`go vet`、`golangci-lint` 均通过（lint 0 issues）；`govulncheck` 未发现可达漏洞，另有 1 项未被本项目调用的模块级记录。
- Python 全量 256 项通过（68.278 秒）；另以外部费用/禁用环境变量运行 9 项定向测试，验证统一测试入口隔离用户状态。
- 安装/解包测试 2 项，包含 22 条官网示例；dispatch 43/43；离线 benchmark 执行器 1 项，正确桩 2/2、错误桩 0/2。
- Shell 语法 21 文件；workflow YAML 4 文件；Homebrew/Scoop 清单 2 份及 5 个归档校验和；Ruby 语法通过。
- 仓库敏感信息检查通过；未部署网站、未发布发行包、未写入真实服务账号。

临时日志保存在 `/tmp/makewand-final-*`、`/tmp/makewand-fixed-govulncheck.log`、`/tmp/makewand-delivery-final-*`；这些日志不是发行物。源码中保留了可持续运行的回归测试。

## 升级行为与验证边界

- 直接云 API 需要显式设置 `MAKEWAND_API_POLICY=allow_paid` 或配置 `"api_policy": "allow_paid"`；远端服务器和第三方 CLI 自身的费用策略仍由其配置控制。
- Go 本地验证最高 Strength 1，需要人工批准才能应用。尚未实现独立、可信的 Strength 2 验收执行器；不能把候选程序的输出当作不可伪造的验收证书。
- 旧版保存的候选需要重新生成，以取得包含权限和删除计划的完整封存记录。旧管理员浏览器会话需重新登录。凭据失效不取消已经通过认证并开始执行的请求。
- Go `setup` 遇到损坏的共享 JSON 会返回错误并保留原文件，避免覆盖 Python 设置；修复该文件后再运行 setup。
- Python 安全候选应用依赖 POSIX 目录句柄。Windows 完整流程请使用 WSL2；原生 Windows 会明确拒绝该操作。已配置 Linux/macOS/Windows 发行入口 smoke，本次仅实际运行 Linux。
- benchmark 桩测试只证明评测执行器能区分正确与错误产物。多模型收益、费用、供应商实时兼容性、跨平台完整流程和生产负载仍需对应的真实实验，不能由这些回归测试推导。

详细契约见 [候选验证与接受](VERIFICATION_CONTRACT.md) 和 [服务端授权与凭据失效](SERVER_AUTHORIZATION.md)。
