# 发布加固修改方案

> 依据：2026-07-20 外部评估逐条核实（master@590ecba）+ codex gpt-5.6-sol ultra 对抗讨论。
> 所有引用行号以 590ecba 为准。分三个 Wave：W0 发布阻塞、W1 高优先、W2 条件项。

## 最终状态：codex 六验签核 **可合并（Alpha）** ✅

四项 backlog 经 5 修复轮（F/F2/F3/F4/F5）+ 6 次 codex gpt-5.6-sol ultra 验收，从
R1 FAIL / R2 PARTIAL / R3 FAIL / R4 FAIL 达到全部 PASS。codex 最终裁决"可合并（Alpha
单实例可信内网）"，无剩余阻断项（已共识的架构边界除外：流式尾账/多实例弱一致/timeout
不真 drain/上游成本不可撤销/--auth-config 静态 token 不持久化）。全量门禁全绿。

## 实施状态（2026-07-20，branch `release-hardening`）

W0 + W1 + W2 全部实施完成，选用**方案 A（Server 撤生产口径）**。全量门禁绿：
gofmt / build / vet / test_gate / test_race(28 包) / golangci-lint(0) / govulncheck(1.25.12 下 0 漏洞) / E2E。

**W1.4 StrictAccounting**：首版仅做了 `Logger.Log` 返回 error + `UsageLogErrorHandler` 钩子
（消灭静默漏账），把"请求级阻塞"列入 backlog。**该 backlog 已在 R 轮实现**（见下）。

## R 轮：剩余 backlog 全部实施（2026-07-20，同分支）

应"完成全部剩余项"要求，四项 backlog 全部落地并加测试：

- **R1 请求级记账阻塞**：`HTTPHandlerOptions.StrictAccounting` + `--strict-accounting`。非流式请求在写**成功**
  响应前记账；失败则 503 `accounting_unavailable`、不写响应体（保证"无未记账的成功响应"，非"永不计费"——
  上游已发生成本不可撤销）。流式响应已在线上无法回收，仍尾部记账+告警（架构固有边界，见 SERVER_ALPHA）。
  `usageLogged` 哨兵防重复记账。参见 F 轮/F2 轮修复。
- **R2 token 计数持久化**：`token_usage_counters` 表 + `SQLiteStore.PersistUsageCounters()`/`loadUsageCounters()`；
  serve 周期 flush（60s）+ 关停前 flush（Close 前 defer），启动恢复。跨重启存活；陈旧日/月窗口首用自愈。
  多实例仍为每进程（已在 SERVER_ALPHA 说明）。
- **R3 预算原子预留**：`Router.budgetReserver`（按 scope 的 in-flight 预留）+ `--budget-reservation`（默认 $0.01）。
  admit 在锁内 `ledger + reserved >= budget` 判断并预留，release 在记账后（reservation 永不早于 ledger 落账被丢）。
  并发超支被 estimate 上界约束（残差=estimate，已说明）。
- **R4 TUI 真月度预算**：`MonthlyLedger`（持久化到 `~/.config/makewand/monthly_spend.json`，按自然月滚动，
  独立于会话 CostTracker）。budget 策略改读月度台账→跨 `/clear`/新会话/重启存活，月初归零。

## R 轮 codex 验收 FAIL → F 轮修复（2026-07-20，同分支）

codex gpt-5.6-sol ultra 首轮验收 R1/R3/R4 FAIL、R2 PARTIAL，均属实，已修：

- **F-R4（R4 是致命 bug）**：`monthly.Add` 原只接在 `recordKnownUsage`（错误分支），常规成功消费直接写 session cost 绕过月台账 → 月度预算永不累加。修：`recordKnownUsage` 成为**唯一** cost sink，所有成功/错误/流式路径统一走它；累加条件改 `cost>0`（订阅报 0，顺带修 ensemble 混合 API/订阅误计）。加 TUI 包级 `TestMain` 隔离真实 `~/.config/makewand`（原测试污染真实 monthly_spend.json，已清理）；加成功路径累加测试。
- **F-R3（TOCTOU）**：原 ledger 读在锁外→整批旧快照请求可重吃已消费 headroom。重构 reserver 为**内存权威 committed**（首次按 scope 从 ledger 播种一次，之后随 settle 维护），admit 全程锁内读 committed+reserved；release→settle(actualCost)（释放预留+把真实成本折进 committed，永不早于落账）。overshoot 文案改为"残差≤每并发请求(真实成本−预留估算)"。
- **F-R1（strict 覆盖/nil logger/duration）**：strict+nil UsageLogger 原假成功→改 fail-closed 503；serve 加启动校验（--strict-accounting 无 sink 直接报错）；strict 写账前补 `usageEntry.DurationMS`；承诺改精确表述"无未记账的成功响应"(非"永不billed-but-unrecorded"，上游已发生成本不可撤销，已说明)。加 nil-logger + /v1/responses 端点 strict 测试。
- **F-R2（flush 生命周期）**：周期 flusher 改专用 ctx+WaitGroup，shutdown 时 cancel+join 后再终刷（消除并发 Persist 回退旧快照）；终刷移到 `server.Shutdown` 排空 in-flight 之后；`PersistUsageCounters` 改逐 token 续跑+errors.Join（与注释一致）；终刷错误不再静默；SERVER_ALPHA 明确 counter 持久化只覆盖 state-DB token（--auth-config 静态 token 仍内存态）。

## F2 轮：codex 复验（R4 PASS/R1 R2 PARTIAL/R3 FAIL）后二次修复

codex 二验确认 R4 PASS 及 TOCTOU/flush 顺序/nil-logger 已修，但发现更多 R3/R2/R1 真 bug，已修：

- **F2-R3**：(1) admit 改**投影准入**——`committed+reserved+estimate>budget` 才拒（原先没算本次预留，导致 committed=.99+estimate=.5 仍放行超支）；不变量变为"admit 后 committed+reserved≤budget→真实成本≤预留则永不超支"。(2) reserver state 加**月份维度**（scope key = `YYYY-MM|project:id`），长驻进程跨月自动滚动、在途请求各带自己的 key。(3) **NaN/Inf/负数**经 `sanitizeCost` 归零，`--budget-reservation` 启动校验有限非负。(4) project-only token 解析出的**父组织写入 usage entry**（durable 归账，重启后 org 播种能读到）；grant org 与 project 父 org **不一致时 fail-closed** 403 scope_conflict。
- **F2-R2**：(1) `parseUsageTime` 改返回 error，`loadUsageCounters` 遇**损坏时间戳 fail-closed**（拒启动，不再静默清零计数）。(2) `Shutdown` **超时告警**（在途请求可能未 drain、尾账可能丢）；SERVER_ALPHA 明确"仅 clean shutdown 保证终刷一致"。
- **F2-R1**：(1) 清理 CLI help 与 plan doc 旧强 claim（"no billed-but-unrecorded"→"无未记账的成功响应"）。(2) DurationMS 语义：0=真实 sub-ms（不伪造钳制），加 sleepyProvider 测试证明 strict 记账 entry 的 DurationMS 已填充（非硬编码 0）。
- 新测试：投影准入/sanitize/committed 持久、org 不一致 403、corrupt-time fail-closed、strict entry 填充。

## F3 轮：codex 三验（R4 PASS/R1 PARTIAL/R2 R3 FAIL）后第三次修复

codex 三验确认核心设计正确（投影准入/父组织/月份 key/DurationMS 语义/三项架构边界），但发现 7 条更细的 fail-open/race，已修：

- **F3-1（R2 竞态）**：`serverauth.SQLiteStore` 加 `mutationMu`，`Issue`/`Revoke` 整个 mutation+reload 串行化——并发签发不再因乱序 reload 丢新 token / 复活已撤销 token。加并发 Issue 测试。
- **F3-2（R3 seed 毒化）**：seed 改**逐 entry** `sanitizeCost` 求和（原先 `SummarizeEntries` 先求和后整体清洗，负数抵销/NaN 清零合法消费）；且 `logHTTPUsage` 入账前 sanitize CostUSD（ledger 永不写 NaN/Inf/负）。
- **F3-3（公共 API 未闭合）**：`reserveTeamBudget` 入口 `sanitizeCost(estimate)`（库调用者绕过 CLI 传 NaN 也安全）；`serverteam` org/project 预算加 finite 校验（原只判 <0，NaN 可绕过）。
- **F3-4（双时钟）**：预算月份改用 `usageEntry.Timestamp`（handler 起点、与 durable ledger 同一时钟）——慢请求跨 UTC 月界不再"内存记新月、持久记旧月"。
- **F3-5（whitespace）**：`parseUsageTime` 去掉 TrimSpace——只有精确 `""` 是合法零值，`"   "` 严格解析失败→fail-closed。
- **F3-6（不可用 sink 假成功 + Close 竞态）**：`JSONLLogger`/`SQLiteStore.Log` 对 nil/closed 返回 error（strict 不再假成功）；文件句柄检查移入锁内（消除与 Close 的数据竞态）；audit logger 同样修竞态。
- **F3-7（日志/文档）**：shutdown 超时不再无条件打印"gracefully"；清理 `HTTPHandlerOptions` GoDoc 与测试注释里"usage 总在响应后记账"的旧表述。

## F4 轮：codex 四验（R4 PASS/R1 R2 PARTIAL/R3 FAIL）后第四次修复

codex 四验确认 F3 全部落地无回归，剩 4 条（2 真 bug + 2 公共 API 契约边缘），已修：

- **F4-#2（shutdown 数据竞争）**：`shutdownTimedOut` 改 `atomic.Bool`——35s fallback timer 分支不接收 `shutdownDone`、无 channel happens-before，plain bool 有竞争。
- **F4-#3（SQLite 月初首秒漏读 → 预算 fail-open）**：timestamp 存储改**固定宽度** layout（9 位纳秒 `sqliteTimeLayout`），since/until 边界同格式——变宽 RFC3339Nano 下 `…00.123Z` 字符串序 < `…00Z`，`>= 月初` 会漏掉月初亚秒消费，重启 seed 后可重得额度。`time.Parse(RFC3339Nano)` 仍兼容回读旧行。新测试 `TestSQLiteMonthStartBoundaryIncludesSubSecond`。
- **F4-#4（typed-nil Reader fail-open）**：`SQLiteStore.Load`/`JSONLReader.Load` 对 nil receiver 返回 error（预算读到它 fail-closed 503，而非 seed 0）。新测试。
- **F4-#1（strict 被 observer sink 假成功）**：加 `Durable() bool` 标记——`WebhookNotifier.Durable()=false`（观察者不持久化），`usageMultiLogger.Durable()`=任一成员持久化；`recordUsageStrict` 拒绝非持久化 sink；serve 启动校验 `--strict-accounting` 需持久化 sink（alert webhook 单独不算）。新测试 `TestStrictAccountingRejectsObserverOnlySink`。

## F5 轮：codex 五验（1/2/4 PASS，3 PARTIAL）后修最后一个回归

codex 五验判 strict/shutdown/typed-nil-reader 三项 PASS，仅剩 1 个**我在 F4-#3 引入的回归**：固定宽度 timestamp 存储对**旧库变宽行 + inclusive `Until` 等值查询**不兼容（旧行 `…00.123Z` vs 新固定边界 `…00.123000000Z` 在 TEXT `<=` 下为 false，等值行被漏）。已修：

- **F5**：存储和 `Until` 边界**回退 RFC3339Nano**（旧/新行同格式，等值 `<=` 恢复，消回归）；固定 9 位宽度只用于 `Since` 下界（`sinceBoundaryLayout`）——它严格修复月初亚秒 seed 漏读，且对任何行都不会比旧 RFC3339Nano 边界少选（只多选边界秒的亚秒行）。新测试 `TestSQLiteUntilBoundaryIncludesEqualInstant` + 保留月初 Since 测试。

全部门禁复跑绿：gofmt/build/vet/test_gate/test_race(28)/golangci-lint(0)/govulncheck(1.25.12 下 0)。

## 前置决策：Server 定位（影响 Wave 2 是否执行）

- **方案 A（推荐，codex 共识）**：撤掉生产口径。`docs/DEPLOY_PRODUCTION.md` 降级为"单租户内网部署（Alpha）"，与 `docs/SERVER_ALPHA.md` 对齐。Wave 2 的 2.1/2.3 转为文档披露的已知限制。
- **方案 B**：保留 "Production Deployment" 口径。则 Wave 2 全部升级为发布阻塞，必须先修。

本方案按 A 编写，B 的增量在 Wave 2 标注。

---

## Wave 0 — 发布阻塞（P0，合计约半天）

### 0.1 Go 工具链升级（GO-2026-5856，govulncheck 实测可达）

| 文件 | 改动 |
|---|---|
| `go.mod:3` | `go 1.25.0` → `go 1.25.12` |
| `deploy/Dockerfile:1` | `FROM golang:1.25 AS build` → 钉到 `golang:1.25.12`（或 digest） |

- CI/Release 均 `go-version-file: go.mod`（ci.yml:50、release.yml:23/63），改 go.mod 即全链生效；本机 1.26.4 开发不受影响（go 指令是下限）。
- 升级后**重建并重发全部制品**（现有 release 二进制带旧 stdlib）。
- 验收：`govulncheck ./...` 0 可达漏洞；`make build`；`scripts/test_gate.sh` 绿。

### 0.2 govulncheck 双门禁（CI + Release 都要）

- `.github/workflows/ci.yml`：新增 job `vulncheck`——setup-go（go-version-file）→ `go install golang.org/x/vuln/cmd/govulncheck@latest` → `govulncheck ./...`。
- `.github/workflows/release.yml`：build 之前加同一 step（发布链独立把关，不依赖 CI 曾经绿过）。
- 可选：`Makefile` 加 `vulncheck` target 供本地跑。
- 验收：CI 绿；将 go.mod 临时降回 1.25.0 时门禁必须红（负向验证一次）。

### 0.3 `.env` 排除

- `.gitignore` 追加：`.env`、`deploy/.env`。
- `.dockerignore` 追加同样两行（构建上下文=仓库根，compose 指示密钥放 `deploy/.env`，当前会被 `COPY . .` 吸进 build-stage 层缓存）。
- 验收：`git check-ignore deploy/.env` 命中；构建上下文验证不含 .env。

---

## Wave 1 — 高优先（P1，合计约 3–4 天）

### 1.1 quota 生命周期重构（替代"serve 里生硬调 Start"的补丁方案）

现状问题：`internal/model/router.go:169` 构造函数里 `go snap.Refresh(context.Background())`（一次性、不可取消）；`snap.Start(ctx)` 全仓零调用；且现有 `Start` **非幂等**（重复调用开多个 ticker，router/quota.go:335/399）并同步阻塞首刷（每源最多 20s）。

设计（codex 共识：显式生命周期，composition root 持 ctx）：

1. `router/quota.go`：
   - `Start` 加幂等闸（`atomic.Bool started`），重复调用 no-op；ctx 取消停 ticker（现有行为保留）。
   - 新增 `LoadCache()`：复用 `loadQuotaCache`（quota.go:372 已有）做纯磁盘读，无网络。
2. `router/router.go`：新增 `(r *Router) StartQuotaRefresh(ctx context.Context)`（转发 snapshotter.Start，幂等可取消）；可选 `RefreshQuota(ctx)` 同步单刷给需要 readiness 的调用方。
3. `internal/model/router.go`：删除 `:169` 的裸 goroutine；构造时改为同步 `snap.LoadCache()`（TTL 内缓存兜底首请求，冷启动空快照=中性排序本就确定，e2e 已验证）。**构造函数从此零网络 I/O、零后台 goroutine。**
4. 接线：
   - `cmd/makewand/serve.go`：用 signal-derived ctx 调 `StartQuotaRefresh`。
   - TUI 长会话入口（`tui.Run`）：用 program 生命周期 ctx 调用。
   - headless 单次 prompt：不接（磁盘缓存已覆盖）。
5. 测试：Start 幂等（注入假 QuotaSource 计 ticker 触发次数）；构造无网络断言；`TestE2E*` 回归（isolatedCLIEnv 的 HOME 隔离使缓存路径也隔离，hermetic 不破）。

验收：serve 长跑配额按 interval 刷新；eval/bench 循环反复 NewRouter 无 goroutine 泄漏；原"测试临时目录清理竞态"消失（goroutine 不再逃逸测试生命周期）。

### 1.2 文档/宣传对齐（热重载 + Server 定位）

- `README.md:29` 特性行、`:222-224`：撤 "hot-reload (30s polling)"，改为"启动时加载 routing.json，修改后重启生效；库嵌入者可显式调用 `WatchOverrides`（已弃用）"。中英同步。
- `router/reload.go`：`WatchOverrides`/`WatchOverridesInterval` GoDoc 加 `Deprecated: 计划 v0.3 删除（见 MAKEWAND_OPTIMIZATION_PLAN.md:512）。` 不接入 CLI（v0.3 就删，不值得新增 goroutine 语义负担）。
- `docs/DEPLOY_PRODUCTION.md`：标题与首段改"单租户内网部署（Alpha）"，全文清理 "Production" 承诺措辞，与 SERVER_ALPHA.md:3 对齐。
- `deploy/systemd.makewand.service` 相关文档：备份说明明确状态分布在**两处**（`/var/lib/makewand` + `/etc/makewand/server_auth.json`，单 state_dir 脚本备不全）。
- 验收：`grep -ri "hot-reload\|30s polling" README*` 无宣传残留；`grep -r "Production Deployment" docs/` 无。

### 1.3 SQLite PRAGMA 每连接生效

现状：`serverdb/sqlite.go:26` 用 `db.Exec` 设 PRAGMA，只作用于池中一个连接；`busy_timeout`/`foreign_keys` 是连接级，新连接 timeout=0、FK off。

- 改 DSN（modernc 驱动）：`sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")`；`journal_mode=WAL` 是库级持久设置，保留一次性 Exec 或同放 DSN 均可。
- 测试：`db.SetMaxOpenConns(4)` + 并发查询 `PRAGMA busy_timeout` / `PRAGMA foreign_keys`，断言每连接命中。
- 验收：新测试绿；`scripts/test_race.sh` 绿。

### 1.4 usage 账本 fail-open 收口

现状：`serverusage.Logger` 接口 `Log(Entry)` 无 error 通道（usage.go:77），SQLite INSERT / JSONL Encode 失败静默丢账 → 预算永久少记。

- 接口改 `Log(Entry) error`（波及：`serverusage/usage.go`、`serverusage/sqlite.go`、`serveralerts/notifier.go`、`cmd/makewand/serve.go` 的 usageMultiLogger 用 `errors.Join` 聚合）。仓库处于 v0.x，公开接口破坏可接受，CHANGELOG 注明。
- `router/http.go` `logHTTPUsage`（:1402/:1412 调用处）：失败 → trace + 失败计数 + alert webhook 告警。
- 新增 `ServerOptions.StrictAccounting`（默认 false）：true 时记账失败返回 5xx，不再"响应成功但漏账"。SERVER_ALPHA.md 说明预算强一致需开启。
- 验收：注入失败 store 的单测——默认模式响应成功且告警触发；strict 模式 5xx。

### 1.5 备份/恢复一致性（复用现成死代码 `internal/backup`）

现状：`internal/backup` 已有 `BackupDatabaseClean`（checkpoint）+ Manifest/SHA256 校验，但**零非测试接线**；`scripts/backup_state.sh:10` 直接 tar 运行中的 state.db（且 :15 失败回退会打包整个目录）；`restore_state.sh:7` 直接解压覆盖，无校验无原子切换。

- 新增子命令：
  - `makewand state backup <out.tar.gz> [--data-dir] [--auth-config]`：checkpoint（或 `VACUUM INTO` 临时文件）+ auth/audit/usage 文件 + manifest 打包，覆盖 /var/lib + /etc 两处。
  - `makewand state restore <archive> [--target]`：临时目录解压 → manifest 校验 → 原子 rename；要求服务已停（flock 或 pid 检查）。
- `scripts/backup_state.sh` / `restore_state.sh`：改为调用上述子命令的薄包装；删除 :15 的回退分支。
- 验收：写入压力下备份→恢复→`PRAGMA integrity_check` = ok 的集成测试。

### 1.6 预算检查 fail-closed + 父 org 解析

- `router/http.go` `checkTeamBudget`（:1226）：`GetProject`/`GetOrganization` 出错时（:1232 现为放行）→ 返回 503 `budget_unavailable`（或与 StrictAccounting 联动）。
- project-only token：由 `project.OrganizationID`（serverteam/team.go:34 已有字段）解析父 org 一并检查，堵住 project token 绕过父组织预算。
- 验收：单测——store 注错→拒绝；project token 触发父 org 预算上限。

---

## Wave 2 — 条件项（P2；选方案 B 则升级为阻塞）

### 2.1 token 级配额计数持久化
现状：`serverauth/auth.go` grantUsage 小时/日请求数+日/月费用全内存（sqlite_store.go 只存规则；auth.go:620 附近 TODO 自认），重启清零。
- 方案 A 下：SERVER_ALPHA.md 明示"token 限额为重启重置的软限额"，代码不动。
- 方案 B 下：计数周期性 + 优雅停机 flush 到 state DB，启动恢复；费用类可从 usage store 重算兜底（请求数无来源，必须持久化）。

### 2.2 TUI MonthlyBudget 语义
现状：`internal/tui/app_budget.go` 用 `cfg.MonthlyBudget` 对比 `SessionTotal`（cost.go），`/clear` 清零——名不副实。
- 推荐：`costEntry` 加 timestamp 并入 session 持久化（session.go:168 state.Costs 通道已存在），`BudgetStatus` 按自然月跨会话聚合。
- 最小替代：配置改名 `SessionBudget` + 文案"本会话预算"。

### 2.3 预算原子预留（并发超支）
- `checkTeamBudget` check-then-act → SQLite 事务内原子预留（估算成本 upsert 当月累计，完成后校正）。
- 方案 A 下可暂缓，SERVER_ALPHA 披露"并发窗口内可轻微超预算"。

---

## 总验收门禁（每个 Wave 完成后跑）

1. `scripts/test_gate.sh` + `scripts/test_race.sh`（28 包）全绿
2. `golangci-lint run` 0 issue；`gofmt -l .` 空
3. `govulncheck ./...` 无可达漏洞
4. `grep` 验证：README 无热重载宣传、docs 无 Production 承诺残留
5. `TestE2E*` 全绿（1.1 改动重点关注 hermetic 测试）

## 工作量与顺序

| 项 | 预估 | 依赖 |
|---|---|---|
| 0.1–0.3 | 半天 | 无，先做 |
| 1.1 quota | 1 天 | 无 |
| 1.2 文档 | 半天 | 定位决策 |
| 1.3 PRAGMA | 2 小时 | 无 |
| 1.4 账本 | 半天 | 无 |
| 1.5 备份 | 1 天 | 无 |
| 1.6 预算 | 2 小时 | 可与 1.4 同 PR |
| Wave 2 | 决策后排期 | 定位决策 |

## 风险与回滚

- **1.1 行为变化**：构造不再自动网络刷新——磁盘缓存（TTL 120s）兜底 + 显式 `RefreshQuota` 出口；若下游依赖旧行为，回滚点是恢复单行 goroutine。
- **1.4 接口破坏**：`serverusage.Logger` 是公开包接口；v0.x 允许，CHANGELOG + router/README 注明。
- **1.5 新子命令**：脚本保持同名薄包装，外部调用方（systemd/文档）无感。
