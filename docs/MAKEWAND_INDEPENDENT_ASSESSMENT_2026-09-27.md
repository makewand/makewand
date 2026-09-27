**Makewand 当前版本独立深度评估**

> 本文保留修复前的基线评估与反例。后续修复及验证记录见 [修复清单](ASSESSMENT_FIXES_2026-09-27.md)。下述“当前版本”均指评估时的基线提交。

评估日期：2026-09-27。基线提交：`806428cd0ff9b065459d3f6c91731a853197920d`。本报告评估修复提交 `0dc4147` 之后的代码，不把旧审计报告中的缺陷直接视为仍然存在。本次只增加评估文档，没有修改产品实现。

**结论：已有实质工程基础，适合受控的个人试用；当前版本尚不满足默认无人值守交付、公开多租户服务或完整稳定发行的要求。最重要的阻碍是授权边界、验收结果与产物的一致性，以及安装包完整性。**

| 使用目标 | 当前判断 | 主要原因 |
|---|---|---|
| 开发者在熟悉的项目中辅助编程 | 有条件可用 | 路由、回退、候选、测试、审查机制均有实现，仍需人工验收 |
| 自动接受并交付 AI 修改 | 暂缓 | 最高强度验证可接受错误产物，race 可忽略裁判拒绝 |
| 执行不可信仓库 | 暂缓作安全承诺 | 官网入口可先执行当前目录代码，网络隔离契约与实现不一致 |
| 对外多租户托管 | 不通过本次上线评估 | 限定租户的管理权限可以越界，成员停用未使旧 token 失效 |
| 面向普通用户发布稳定安装包 | 不通过本次发布评估 | Release 只含 Go 二进制，核心命令依赖未打包的 Python 引擎 |
| 宣称多模型优于单模型 | 证据不足 | 没有可复验的完整对照实验与质量、时延、资源联合数据 |

**1. 评估方法与边界**

本次分别审阅 Python 编排与候选工作区、Go 验证与服务端授权、安装发布与文档，并执行全量测试和针对关键保证的最小反例。Go 验证器反例额外在真实 Bubblewrap 中运行，报告中的 `isolated=true` 来自执行结果。服务端反例使用临时 SQLite、内存 HTTP 请求和假的模型 provider，不访问真实账号或模型服务。Python 流水线反例替换模型响应，并在临时目录执行固定的无害测试；该项证明流程一致性问题，不声称进行了真实模型质量测试。

未进行付费模型调用的端到端评测、外部生产站点渗透、长时间压力测试、故障注入恢复测试或当前依赖漏洞数据库复核。因此本报告不证明所有供应商 CLI 的实时兼容性，也不对线上服务的实际配置作结论。

仓库跟踪文件中有 252 个 Go 文件、67,388 行，其中测试 26,413 行；47 个 Python 文件、15,626 行，其中测试 5,173 行。这是有明显工程投入的软件，评估重点应是关键保证是否成立，而非功能数量。

**2. 实际验证结果**

| 检查 | 结果 | 解读 |
|---|---|---|
| `go test -count=1 -json -coverprofile=… ./...` | 通过；805 个顶层测试、包含子测试共 977 个 pass 事件 | 以当前源码实际执行，无测试缓存替代 |
| Go 语句覆盖率 | 60.9% | engine 78.1%、router 77.4%、serverauth 69.7%、serveradmin 61.7%、cmd/makewand 41.0% |
| `go test -race -count=1 ./...` | 通过 | 未发现这组测试所覆盖执行路径中的数据竞态 |
| `go vet ./...` | 通过 | 静态检查通过不代表业务安全保证成立 |
| 真实 Bubblewrap 集成测试 | 通过 | `TestEvaluateCandidateFiles_LiveBwrapIsolation` 实际执行 |
| Python unittest，临时配置隔离 | 226 项通过，68.388 秒 | 重定向 config/status/candidate/usage 等路径，未改写 HOME |
| 当前 Go CLI 构建 | 成功 | 另在清理环境后验证发布能力，发现 Python 引擎缺失 |
| 本报告的定向反例 | 多项成功触发 | 表明现有绿灯测试缺少关键行为反例 |

首轮受限环境的 Go 测试因无法监听回环端口而失败，Python 测试还受只读用户配置目录、现有 provider 配置和 Bubblewrap 网络权限影响。排除这些环境因素后的结果如上，不把首轮失败归为产品缺陷。另一方面，标准 Python unittest 入口会触及真实用户状态，`tests/conftest.py` 的 pytest fixture 不会被 unittest 自动使用；测试入口本身仍应集中隔离状态。

证据日志保留于本次会话目录 `/tmp/makewand-assessment-806428c/`：`go-tests-full.json`、`go-coverage-full.out`、`go-race.log`、`go-vet.log`、`python-tests-isolated.log`、`go-repros-bwrap.log`、`service-repros.log`。临时证据路径不属于发行物，核心观察已摘录进本报告。

**3. 优先级最高的发现**

这里的 P1 表示相关功能发布前应修复；P2 表示会造成实际错误或明显削弱可靠性。并非所有问题都对所有部署成立，前提在各项中单列。

**R01 · P1 · 限定租户的管理 token 可越过租户边界，用户管理权限还能扩展为全局管理权限。实测确认。**

位置：`serveradmin/handler.go:412`、`:431`、`:455`、`:930`；`serveradmin/session.go:157`、`:173`。

用户列表过滤和用户修改只检查 grant 的 `UserID`，没有按 `OrganizationID` / `ProjectID` 限制目标用户。持有某组织的 `admin:users:read/write` token，在没有 UserID 限制时可查看另一个组织的用户并修改其密码。进一步，用户 role 更新允许写入全局 `admin`；浏览器登录随后根据全局用户角色创建包含 `AllScopes()`、不携带租户限定的会话。

临时数据库中建立两个组织和各自的用户，观察到：

```text
cross_tenant_list status=200 contains_bob=true
cross_tenant_password status=200 changed=true
tenant_to_global_admin role_status=200 browser_status=200 auth=true org="" global_token_write=true
```

前提是管理员已经委派了带相应 users scope 的 token；不是匿名未认证攻击。风险在于组织范围和 capability 范围没有覆盖后续用户角色、浏览器会话的授权转换。

修复验收：以 actor、目标用户、组织/项目 membership 和 action 共同授权；租户管理员不得修改全局用户角色或跨租户密码；只有明确的全局管理员权限可授予全局 admin。测试必须覆盖组织限定、项目限定、用户限定及上述组合，并覆盖从 token 到浏览器会话的转换。

**R02 · P1 · 成员停用后旧 token 继续有效，管理员密码修改也不撤销旧浏览器会话。实测确认。**

位置：`serveradmin/team.go:498`、`:580`；`router/users.go:598`、`:611`；`router/http.go:255`、`:1378`；`serveradmin/handler.go:484`；`serveradmin/session.go:157`、`:215`。

membership 的有效性在登录签发阶段检查，修改 membership 后没有相应 token 撤销，也没有在后续模型请求处重新检查。实测同一用户停用组织 membership 后，新登录被拒绝，旧 token 仍成功到达 mock provider：

```text
membership_deactivation disable_status=201 new_login_status=403 old_token_chat_status=200 stub_calls=1
```

管理员密码重置调用 `RevokeByUserID` 撤销 API token，但浏览器 cookie 仅含签名声明和到期时间。认证只检查用户仍活跃且角色仍是 admin，没有会话版本或密码变更时间校验：

```text
old_browser_cookie_after_password_change change_status=200 still_valid=true
```

这影响成员离职、租户移除和账号失陷后的响应。修复验收：明确全部凭据类型的撤销语义；membership 降权/停用及时生效；密码重置可使旧管理员 cookie 失效；以授权版本或可撤销 session 存储实现统一失效。

**R03 · P1 · 测试/审查通过的内容和最终交付内容仍可能不同。Go 与 Python 均实测确认。**

位置：`internal/engine/workspace_verify.go:460`、`:464`；`makewand/orchestrator.py:1287`、`:1294`、`:1301`；race 同类路径在 `:2071`。

Go 当前先执行验证，再从可写的验证目录读取 `VerifiedFiles` 和计算 `VerifiedDigest`。固定反例在测试启动时修改源码，已编译测试仍正常通过；随后读取的所谓 verified 内容已是无法编译的文本：

```text
post_test_mutation passed=true strength=2 baselineTests=true noTestsRan=false
isolated=true
verified_math="this is not Go code\n"
```

Python 在测试前提取 diff，测试之后仍把旧 diff 送审。真实临时 pytest 用例修改 `app.py` 后，stub reviewer 看见 `APPROVED = 2`，流水线返回成功，但落盘为 `POST_TEST_UNREVIEWED = True`。允许测试生成或改写文件本身很常见；问题是没有在阶段边界重新核对被验证、被审核和被交付的对象。

新增 digest 字段有帮助，但对测试后状态计算 hash 并不能追溯证明它已经通过测试。修复验收：封存明确的候选清单与文件模式，对验证前后输入状态做完整性检查；测试输出放入独立目录；内容发生变化必须重新验收；最终 apply 复核同一版本的验收凭据。普通代码生成测试也必须覆盖此场景。

**R04 · P1 · Go 的 Strength 2 不能保证基线测试实际执行。真实 Bubblewrap 中两种独立反例均成功。**

位置：`internal/engine/sandbox.go:229`；`internal/engine/workspace_verify.go:591`、`:1021`、`:1058`、`:1118`。

第一种是测试入口替换。基线为有真实测试的 Go 项目；候选把实现改错，同时新增含 `scripts.test="true"` 的 package.json。基线仍被判定“有可信测试”，候选目录却优先选择 npm，直接得到最高强度：

```text
runner_switch passed=true strength=2 baselineTests=true noTestsRan=false
isolated=true
runner=npm test
verified_math="package fixture\nfunc Add(a,b int) int { return 0 }\n"
```

第二种是输出证明不足。候选普通 Go `init` 输出 `--- PASS:` 并退出，未新增 TestMain，也没有执行基线测试，当前 `goTestOutputRanTests` 仍视为已跑测试，返回 `strength=2`。此前对新 TestMain 的特例防护不能覆盖任意候选执行入口。

修复验收：从可信基线确定测试计划，并使新增工程文件不能改写该计划；多语言项目明确组合哪些测试。实际测试执行证据不能只依赖候选进程可以输出的文本。采用结构化结果、匹配预期基线用例和隔离的验收驱动可提升可信度，但单独换成 JSON 解析也不等于解决全部伪造问题。

**R05 · P1 · race 裁判明确拒绝两个方案，仍会产生默认胜者并返回成功。实测确认。**

位置：`makewand/orchestrator.py:2136`、`:2147`、`:2158`。

当两个候选实现和本地测试均通过，裁判未被正则识别为明确推荐 A/B 时，代码按耗时选胜者。固定裁判输出“两套候选方案均存在严重安全缺陷，拒绝采纳任何方案，需要修复后重新审查”，仍观察到 `exit_code=0`、`winner="A"`。保存下来的 winner 可被默认 apply 选中。

修复验收：明确区分通过、拒绝、未决；使用结构化裁决并检查 defects；只有两个方案都被明确接受、且裁判允许等价选优时，才以耗时打破平局。裁判拒绝和失败不能降级为成功。

**R06 · P1 · 官网安装的启动器可先执行工作目录中的同名 Python 包。实测确认。**

位置：`site/install.sh:58`；对照 `bin/makewand:10`。

官网启动器设置 PYTHONPATH 后执行 `python3 -m makewand.cli`。Python 的当前目录查找仍优先于该路径；用户在含同名 `makewand` 包的不可信仓库内运行命令时，参数解析及 repo-trust 防护前就可以执行仓库代码。临时假包只打印一个标记，官网入口等价命令输出该标记，正规源码 `bin/makewand` 在同目录正常输出版本。

前提是使用官网安装器生成的启动器，并进入含特制同名包的目录。修复验收：执行安装目录中的确定入口，并显式固定导入根；在恶意同名包目录中测试 `--help`、`--version` 和 `--repo-trust untrusted`，均必须进入真正的产品入口。

**R07 · P1 · Release/Homebrew/Scoop 发布的二进制缺少核心 Python 引擎。实测与发布配置共同确认。**

位置：`.github/workflows/release.yml:91`；`scripts/gen_package_manifests.sh:93`；`cmd/makewand/main.go:81`、`:214`；`scripts/release_contract.sh:38`。

release tar/zip 只包含 Go 二进制，但 `run/review/race/status/probe` 等主打命令会委派到独立 Python 安装。将当前编译的 Go 二进制置于临时目录，在无旧安装路径的清理环境中执行 `run --help`，退出 1 并报 `Python engine could not be located`。

同一环境的 release contract 仍显示通过，因为它主要检查 `new/chat/serve`；部分子命令 help 失败只打印 warning。已有源码和用户目录安装会掩盖这个问题。

修复验收：统一 Python/Go 分发布局、版本和运行时依赖；在没有源码、没有旧安装的环境中解包后执行完整主命令 smoke。安装包应能按公开的依赖约定独立工作，发行契约必须覆盖 README 中的主路径。

**4. 其他已确认的可靠性与产品契约问题**

| 编号/优先级 | 发现与证据 | 修复验收 |
|---|---|---|
| R08 / P2 | Python `candidate.py:507` 和 Go `project.go:351` 固定写 0600。两条链均实测把已有 0755 脚本变成 0600；Python 回滚路径也未恢复模式 | 候选与备份清单包含受支持的权限元数据；apply/rollback 保留执行位；区分新文件默认权限与原文件权限 |
| R09 / P1，安全契约 | `SECURITY.md:10` 承诺测试步骤 `--unshare-net`，但 `internal/engine/sandbox.go:165` 对 tests 返回允许网络；`verify_isolation_test.go:258` 还将该行为设为正确结果 | 如承诺网络隔离，应在新网络命名空间中提供测试所需 loopback；如选择允许网络，应明确策略及用户可见授权。共享宿主网络同时保留出站及宿主回环访问能力 |
| R10 / P2 | `orchestrator.py:296` 无条件向 npm test 追加 `--passWithNoTests`；原生 Node 测试直接运行退出 0，经 Makewand 退出 9。`:273` 仅凭 tests/ 目录又将纯 JS 工程判为 Python，pytest 退出 5 | 默认执行项目自有测试命令；按语言及配置检测，不把目录名当语言证据；跨 Jest、Node test、其他 runner 做兼容验收 |
| R11 / P2 | `health.py:175` 缩短相对 resets_at 却保留原 updated_at，load/save 都调用 sanitizer。11:00 记录 in 2 hours，12:00 连续两次处理，第一次剩 1 hour，第二次立即变 healthy | 将 reset 存为绝对时间或同步更新相对时间基准；使用假时钟检验重复读取/保存的幂等性 |
| R12 / P2 | 官网写 `local_model_enabled:false` 等字段，但运行时只读取 enabled_providers，默认均为 True（`site/install.sh:79`、`config.py:219`）。mock 本地服务可用时 local 仍进入可用状态 | 配置 schema 统一，安装后以运行时读取结果验收；未知字段有提示或迁移 |
| R13 / P2 | README:18 声称“不产生第三方 API 扣费”；`config.py:120` 自动读取 API key，`providers/claude.py:63`、`:138` 有订阅到 API 回退，相关测试明确支持此行为 | 提供明确的订阅专用/允许付费回退策略，并准确展示当前模式。前提是环境或配置已有可用 API key，不是无 key 扣费 |
| R14 / P2 | 官网文档包含未实现的 --provider/--review-by/--models/--target，实测 exit 2；推荐 pip install -e . 却无打包元数据；公网 serve 示例缺少 TLS 配置而被正确拒绝（`site/docs.html:199`、`:242`、`:254`、`:265`、`:282`） | 将公开示例纳入 CLI smoke，文档从同一命令契约生成；保留服务端 TLS 拒绝机制，修正文档 |

R09 是代码和现有测试直接确认的网络策略，不将其夸大为已实测的数据外泄。对当前挂载树中的 Unix socket、文件路径竞态、资源耗尽等进一步风险，本次没有完整重做所有旧报告的攻击矩阵，不用旧结论填充确认清单。

还有一个安装器组合问题：源码安装以软链接连接仓库 `bin/makewand`，官网安装再用 `cat > ~/.local/bin/makewand` 写入口时会沿现有链接覆盖源码入口。该项来自两段安装器的明确文件操作，未执行破坏性安装验证；应以安全的原子替换实现入口升级。

**5. 架构评价：职责已扩展，但行为契约尚未统一**

当前至少有三套执行机制。Python 承担自然语言命令、工具发现、健康和额度、候选、review/race、自愈。Go 承担 TUI、服务端、独立 provider/router、Power ensemble、项目验证和构建状态机。Shell dispatch 又有自己的 adapter、超时、环境和补丁协议。它们不是清晰的“一个执行内核、多个展示入口”。

这已产生直接可见的语义差异。Python `orchestrator.py:1541` 对未通过审查拒绝交付；Go `internal/engine/buildpipeline.go:215` 在单 provider 时跳过审查，`:226` 将 ReviewError 与 ReviewLGTM 一起送入下一阶段。后续测试可能仍会失败并阻断流程，因此不能把这处简化成“Go 遇到任意错误都交付”；准确结论是审查在两条主链中的门禁语义不同。mode/tier 名字一致不足以保证验收、费用和权限语义一致。

`orchestrator.py` 已有 2,211 行，`router/http.go` 1,688 行，`router/router.go` 1,368 行，`cmd/makewand/main.go` 1,187 行。行数本身不是缺陷，但目前路由、进程执行、判定、提示、事务和计费分散在这些大模块中，会增加跨入口修复遗漏的概率。此次文件模式、审核产物和测试入口问题正反映了这种维护成本。

建议保留 Python/Go 各自合适的职责，先统一版本化任务和结果协议：能力声明、信任级别、网络与费用策略、取消和 deadline、验证三态、产物标识与模式、审核裁决、apply 冲突。分别测试各入口对同一组契约向量的表现，再决定是否合并实现。直接重写成一种语言并不能自动解决这些语义问题。

**6. 产品价值、多模型收益和资源成本**

实际价值主要在统一已有工具入口、故障回退、候选隔离，以及减少人工串接测试与复审的操作。对于拥有多个工具订阅、经常跨工具切换的开发者，这是明确的使用场景。

跨模型复审提供了发现不同错误的机会，但不构成独立正确性证明。模型共享任务、代码和测试，失败可能相关；同一模型的新上下文也不等于独立判断保证。应把确定性外部验收和人工可审计产物置于最终交付依据中。

成本增长是可从控制流推导的，收益则尚缺实验支持。Python 常规路径至少实现和审查两次调用；默认两轮修复时，在没有故障回退的情况下可达到六次调用。race 的主要耗时结构约为 `max(生成 A, 生成 B) + 测试 A + 测试 B + 裁判`，不是第一个候选完成便交付；高负载下生成还可变为串行。Go ensemble 也等待 generator 后执行 judge，不过确实累加了各路与裁判的 usage，这是应保留的设计。

“无额外 API 账单”也不等于没有订阅配额、等待、CPU、磁盘和维护成本。对收益应至少报告任务验收率、回归缺陷率、P50/P95 完成时间、调用与额度消耗、人工修订时间，并区分基础设施失败和模型质量失败。

benchmark 当前不足以支撑优势结论。`benchmarks/README.md` 定义六对象乘五任务，但 `run.sh:56` 对 Makewand 主要打印手动操作说明；当前本地结果只有 direct 工具，没有完整 Makewand 对照。更关键的是 `git ls-files benchmarks` 只列 README、run.sh、results/go.mod，prompt、评分工具和本地报告未版本化；干净检出无法复验实验。不能将计划、预期结论或手工分数当作实测优势。

建议先建立小规模、固定任务、隐藏验收的配对对照：单模型、单模型加测试、跨模型审查、双候选裁判。固定时间/额度预算并重复运行，再判断哪种任务值得额外编排。当前没有证据支持全任务默认使用最复杂模式。

**7. 工程与运维中已经做对的部分**

Go 有合理的 package 边界、SQLite 持久化、scope/grant 机制、usage/audit、额度预留、熔断和回退；候选机制具备临时工作区、测试信任处理、删除提示和宿主执行授权；Python 有候选清单、内容校验、工作区冲突检查、应用锁和失败回滚。这些控制确实存在，本报告所列问题多发生在控制之间的衔接。

CI 的全包 test/vet/race、实际 Bubblewrap smoke、lint、govulncheck，以及 release 的 checksum、cosign 签名和 provenance，说明工程基础已明显超出最小原型。Docker 默认非 root，Compose 将服务端口发布到宿主回环地址，也是合理默认值。签名可以证明发行产物来源，不能弥补发行包漏打引擎。

仍需统一 gate。Makefile 的 test-go 只列九个包，scripts/test_gate.sh 才按 go list 全包运行；tag release 没有执行 Python suite 和安装后的 Python 主路径。Shell dispatch/test.sh 也未进入主 CI。全绿测试之外应维护“产品承诺到反例测试”的映射，让跨租户授权、候选不可变性、裁判拒绝和干净安装成为不可退化的发布标准。

备份脚本、审计和 metrics 的存在不能代替恢复演练、数据库故障与并发负载实测。本次未完成这些专项，因此不据此给出高可用或容量结论。

**8. 建议修复顺序与验收条件**

| 批次 | 应完成的工作 | 可以客观验收的结果 |
|---|---|---|
| 第一批：恢复边界可信度 | R01/R02 授权和撤销；R03/R04 产物验证；R05 裁判拒绝；R06 入口导入 | 跨租户操作均拒绝；旧凭据按策略失效；错误/变更后的产物不能得到自动交付凭据；拒绝结论不选 winner；恶意同名包不执行 |
| 第二批：恢复安装与兼容性 | R07 完整发行包；R08 文件权限；R09 网络策略；R10 测试 runner | 干净机器完整主路径 smoke 通过；执行位不丢失；网络行为与文档一致；标准 Node/Python/Go 项目正确测试 |
| 第三批：统一运行契约 | R11 quota 幂等；R12/R13 配置和费用；Python/Go/dispatch 结果协议与 CI | 同一策略在各入口有相同成功/失败/未验证、费用、权限与取消语义 |
| 第四批：证明产品收益 | 可复验 benchmark，固定预算和独立验收；故障恢复与资源测量 | 能回答哪些任务提高多少验收率、付出多少时延/额度，以及何时应退回单模型 |

不建议以增加 provider 数量、再叠一层审查 prompt 或再次扩大自动审批范围作为当前首要工作。现阶段更高价值的投入，是让已经承诺的验收、授权和安装行为在反例下也成立。

**9. 复现材料索引与适用限制**

本次临时 Go 验证器 harness：`/tmp/makewand-assessment-806428c/go-repro/main.go`。使用当前模块代码、固定临时项目；`MAKEWAND_EVAL_BWRAP=1` 路径实际运行 Bubblewrap，并得到 R03/R04/R08 观察结果。另一条受控宿主路径只用于对照，不是建议用户关闭沙箱。

Python harness：`/tmp/makewand_core_review_repro.py`，包含文件模式、quota、Node runner、测试后 diff 和 race 拒绝反例；模型返回值和部分环境机制被显式替换，测试数据只写临时目录。服务端 harness：`/tmp/makewand-service-review/repro.go`，仅操作临时 SQLite 和 mock provider；结果记录在 `service-repros.log`。这些材料用于定位并编写产品回归测试，不属于生产执行工具。

本报告的可靠性判断来自上述当前代码和执行观察。修复后应重新运行对应反例并更新结论；不能仅因已有全量测试仍为绿色，就认为这些发现已经消失。
