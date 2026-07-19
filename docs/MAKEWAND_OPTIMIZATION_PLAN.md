# Makewand 完整改造与优化实施方案

状态：Proposed  
日期：2026-07-16  
适用范围：CLI、Router 库、可信代码执行与验证、自托管 Server  
关联文档：[AI 编排 MVP](AI_ORCHESTRATION_MVP.md)

## 1. 执行摘要

Makewand 已具备多 Provider 路由、熔断、配额感知、Power ensemble、TUI、
OpenAI 兼容接口、远程会话、用户与团队管理等能力。但功能扩张速度已经超过
验证、发布和运维治理速度，当前不应继续扩大 Planner、多写 Agent 或 Server
生产承诺。

本方案的核心决策是：

1. 保留并审计现有未提交工作，不执行 reset、覆盖或大爆炸重写。
2. 先恢复可构建、可测试、可发布的可信主干。
3. 将自托管 Server 明确降级为 alpha，并移出默认用户路径。
4. 以完整工作区快照作为候选结果的唯一真相源。
5. 在真正隔离的环境中验证冻结候选，再进行 stale 检查和事务化应用。
6. 将 Provider factory、策略、quota、质量和身份归因全部收归 Router 实例。
7. 任何调用、重试、fallback 和 judge 都必须受统一预算与并发舱壁约束。
8. 只有固定 benchmark 证明收益后，才继续 Planner、DAG 或并行写 Agent。

产品成熟度判断：

| 产品面 | 当前定位 | 本轮目标 |
|---|---|---|
| 本地 CLI | Beta 候选 | 成为默认稳定发布单元 |
| Go Router 库 | Beta 候选 | 实例隔离、身份诚实、兼容迁移 |
| 可信执行与验证层 | Alpha | 完成单 Agent MVP 和客观评估 |
| 自托管 Server | Alpha | 安全止血、可恢复、无生产承诺 |

## 2. 已确认的基线事实

以下事实已经通过仓库检查和本地测试验证：

- 当前未提交工作区无法编译。`legacyFallbackCandidates` 已返回
  `[]fallbackCandidate`，但 `router/router.go` 的旧调用点仍把元素当作字符串。
- 已提交的 `HEAD 42150bc` 在隔离副本中通过全量 test、vet 和 race。
- 基线包含 561 个 Test、3 个 Fuzz、81 个测试文件，总语句覆盖率为 53.2%。
- CI 和 `scripts/test_gate.sh` 遗漏全部 `server*` 包；race 也遗漏 Router 和 Server。
- GitHub latest Release 仍是 `v0.1.10`，而主干已领先 54 个提交。README 描述了
  latest 二进制并不具备的命令。
- 当前候选流程可能在 clone A 中运行 Agent，再将模型声明的部分文件重建到
  clone B 验证；未报告修改、删除、rename、mode 和 symlink 语义会丢失。
- 当前 restricted runner 不是安全沙箱，项目代码仍可能访问宿主文件系统、网络和
  用户环境。
- Provider factory 和路由策略使用包级可变状态，多个 Router 可能串用配置或凭据。
- HTTP request-scoped Router clone 没有保留 quota 与 quota policy。
- SQLite 使用 WAL，但现有备份脚本直接归档 live `state.db`，不能保证一致性。

这些问题意味着当前版本适合本地试用，但不能宣称可信自动执行或通用生产服务。

## 3. 方案讨论后的修正

本方案由仓库检查、内部交叉评审以及两轮 `claude-fable-5` 对抗式讨论共同形成。
最终没有直接采用以下建议：

- 不把 `v0.1.10..HEAD` 的约 3.6 万行已提交差异误当成当前 dirty diff，也不以此为由
  reset 或推倒重做。
- 不直接在可能仍有 Agent 后台进程的工作目录中运行强验证。
- 不把不完整的 `unshare` 调用当作可信隔离后端。
- 不将外部 `sqlite3` CLI 作为唯一备份能力。
- 不立即把 HTTP facade 迁出 `router`，以免制造公开 API 破坏和 Go import cycle。

本轮保持同仓、同 Go module，通过包边界、能力门控和发布门槛完成产品分层。

## 4. 核心不变量

### I1：快照是真相源

候选结果必须来自 Agent 工作区的完整文件系统扫描。模型输出仅作为日志和诊断，
不得决定哪些文件被验证或应用。

### I2：验证对象与应用对象一致

Oracle 验证的 `CandidateSnapshotID` 必须与待应用的 Snapshot 完全一致。Verifier
产生的缓存、测试产物和临时文件只能进入一次性 writable overlay，不能改变冻结 Artifact。

### I3：验收资产不可变

既有可信测试、隐藏验收测试、CI、verifier registry、权限和预算配置默认不可修改。
Agent 新增测试可以增加证据，但不能单独构成强验证。

### I4：应用前必须检查 stale

申请执行时的 `BaseSnapshotID` 必须与应用前的真实工作区重新扫描结果一致；否则进入
`STALE_WORKSPACE`，不得覆盖用户后续修改。

### I5：应用具有可恢复事务语义

多文件写入无法在通用文件系统上保证真正原子，因此必须使用 preimage、journal、
临时文件、原子 rename 和启动恢复，并诚实称为 journaled transactional apply。

### I6：单工作区单写者

同一真实工作区在任一时刻只能有一个写 Agent。并行只能发生在互相隔离的候选工作区。

### I7：Router 状态实例隔离

Provider factory、policy、quota、breaker、usage 和质量统计归属于明确的 Router 实例；
request view 只能共享契约中明确允许共享的状态。

### I8：身份与成本归因诚实

每次 Route 必须记录 logical provider、实际 transport、requested model、actual model、
access type 和 price key。CLI/API alias 不得伪装成另一个模型或价格。

### I9：外部消费先准入

Provider 调用、fallback、retry、ensemble、judge、repair 和 apply 必须先取得并发许可、
预算 reservation 或用户批准。

### I10：安全能力不足时 fail closed

safe/autopilot 缺少符合要求的隔离后端时不得执行命令。macOS/Windows 在没有等价后端时
只能进入 manual，并要求显式 unsafe consent，不能静默降级。

## 5. 目标架构

```text
User Goal
   |
   v
RunSpec + Admission + Budget Reservation
   |
   v
BaseSnapshot --------- immutable acceptance assets
   |
   v
Isolated Agent Workspace
   |  Agent prose is diagnostic only
   v
Complete CandidateSnapshot
   |
   v
Content-addressed Frozen Artifact
   |
   +---- ChangeSet
   |
   v
Oracle: read-only artifact + disposable writable overlay
   |
   +---- failed ----> bounded repair ----> new snapshot
   |
   +---- passed ----> approval
                         |
                         v
                 stale workspace check
                         |
                         v
                 journaled apply
```

建议包边界：

```text
router/                 路由、策略、quota、breaker、identity、attempt budget
providers/              后续逐步放置 Claude/Gemini/Codex/API/CLI transport
internal/snapshot/      manifest、diff、ChangeSet、freeze
internal/runner/        隔离能力契约及平台后端
internal/agent/         AgentExecutor 和 Agent adapters
internal/oracle/        verifier registry、preflight、evidence ladder
internal/control/       run 状态机、repair、approval、apply
internal/runstore/      run/attempt/evidence SQLite ledger
server*/                auth、API、usage、team、admin、backup、runtime
cmd/makewand/           CLI composition root
```

本轮不立即迁移 `router.HTTPHandler()` 和用户相关公开 API。先禁止继续向 Router 增加
Server 逻辑，在 v0.3 再完成破坏性包迁移。

## 6. 最小 v1 数据契约

### 6.1 SnapshotManifest

```go
type SnapshotManifest struct {
    SchemaVersion string          `json:"schema_version"` // makewand.snapshot.v1
    SnapshotID    string          `json:"snapshot_id"`
    RootDigest    string          `json:"root_digest"`
    IgnorePolicy  string          `json:"ignore_policy"`
    Entries       []SnapshotEntry `json:"entries"`
}

type SnapshotEntry struct {
    Path       string `json:"path"`
    Kind       string `json:"kind"` // file, dir, symlink
    Mode       uint32 `json:"mode"`
    Size       int64  `json:"size"`
    Digest     string `json:"digest,omitempty"`
    LinkTarget string `json:"link_target,omitempty"`
}
```

要求：

- 路径规范化、排序稳定，同一目录重复扫描必须得到相同序列化结果和 hash。
- 覆盖 tracked、dirty、untracked、空文件、mode 和 symlink。
- v1 记录已有 symlink，但候选新增或修改 symlink 默认拒绝。
- ignore policy 必须版本化，不能隐式依赖运行机器状态。

### 6.2 ChangeSet

```go
type ChangeSet struct {
    SchemaVersion       string   `json:"schema_version"` // makewand.changeset.v1
    BaseSnapshotID      string   `json:"base_snapshot_id"`
    CandidateSnapshotID string   `json:"candidate_snapshot_id"`
    Operations          []Change `json:"operations"`
}

type Change struct {
    Operation    string `json:"operation"` // add, modify, delete, chmod
    Path         string `json:"path"`
    BeforeDigest string `json:"before_digest,omitempty"`
    AfterDigest  string `json:"after_digest,omitempty"`
    BeforeMode   uint32 `json:"before_mode,omitempty"`
    AfterMode    uint32 `json:"after_mode,omitempty"`
}
```

rename 可以作为展示提示，但正确应用始终按 delete+add 保证，不依赖 rename 推断。

### 6.3 RunSpec

```go
type RunSpec struct {
    SchemaVersion  string        `json:"schema_version"` // makewand.run.v1
    RunID          string        `json:"run_id"`
    WorkspaceID    string        `json:"workspace_id"`
    BaseSnapshotID string        `json:"base_snapshot_id"`
    Intent         string        `json:"intent"`
    AllowedPaths   []string      `json:"allowed_paths"`
    ImmutablePaths []string      `json:"immutable_paths"`
    VerifierIDs    []string      `json:"verifier_ids"`
    Agent          AgentIdentity `json:"agent"`
    Budget         RunBudget     `json:"budget"`
}
```

`VerifierIDs` 只能引用 Makewand 注册的固定 argv 计划，不能接收模型生成的 shell 文本。

### 6.4 状态机

```text
PENDING
  -> PREFLIGHT
  -> RUNNING
  -> SNAPSHOTTED
  -> VERIFYING
      -> FAILED
      -> REPAIRING -> SNAPSHOTTED     # default at most once
      -> VERIFIED
  -> AWAITING_APPROVAL
      -> DISCARDED
      -> APPLYING
          -> STALE_WORKSPACE
          -> APPLY_FAILED
          -> APPLIED
```

每次状态迁移写入 append-only ledger。只有冻结 Artifact 可以恢复验证；未冻结 Agent
执行在崩溃后必须丢弃或在预算内重启。

## 7. 实施路线图

### Wave 0：恢复可信交付

#### PR-01：修复 fallback candidate 编译回归

目标：恢复当前工作区构建能力，不夹带无关重构。

主要改动：

- `router/router.go` 使用 `candidate.name` 和 `candidate.modelID`。
- 对齐 `router/execute.go` 中已经正确的 `fallbackCandidate` 消费方式。
- 增加 legacy primary 不可用、subscription → API alias fallback 回归测试。

完成条件：

- `go build ./...`
- `go test ./...`
- `go vet ./...`

回滚点：单独 revert 本 PR。

#### PR-02：全仓 CI 门禁

目标：消除 Server 与 Router 的测试盲区。

主要改动：

- `scripts/test_gate.sh` 以 `go list ./...` 为包集合真相源。
- CI、release、prelaunch 共用同一组 build/test/vet 门禁。
- race 覆盖 Router、Server、Engine 和 CLI；必要时按包分片，而不是排除。
- 生成 CI 实际包集合，与 `go list ./...` 比较，防止以后再次漏包。

完成条件：所有正式包均在 CI 中执行，10 个 `server*` 包和现有 38 个 Server 测试入闸。

#### PR-03：版本和发布契约单一真相

目标：消除源码、tag、二进制、Release、安装器和文档漂移。

主要改动：

- 新增 `internal/buildinfo`，版本由构建注入，开发构建显示 commit/dirty 状态。
- Makefile 和 release workflow 使用同一 ldflags 目标。
- 新增 release-contract 脚本，验证 tag、`--version`、archive 名和 Release notes。
- 对 latest artifact 运行公开命令的 `--help`，与文档命令清单交叉验证。

完成条件：版本与功能清单 100% 一致；untagged/dirty 构建不能发布正式 Release。

#### PR-04：Server alpha 分层和远程文档修复

目标：保留 Server 代码和测试，但停止暗示它已生产就绪。

主要改动：

- README 默认 Quick Start 仅保留 CLI 能力。
- Server 内容移入 `docs/SERVER_ALPHA.md`，醒目标注 alpha 与不承诺事项。
- 默认帮助中将 Server 管理命令归入 experimental 分组；是否隐藏由兼容性测试决定，
  不依赖只靠文档隐藏风险。
- 远程默认路径仅允许：loopback + SSH tunnel，或 private network/reverse proxy + TLS。
- 删除明文传输密码/token 和把密码放进 argv 的推荐示例。
- 修正 `router/README.md` 关于 per-instance factory 的错误声明。

完成条件：latest 安装后二进制能力与 README 完全一致。

#### PR-05：Server 安全止血

目标：在继续保留 alpha Server 的前提下消除高风险缺陷。

主要改动：

- `cloneView` 保留 quota 和 quota policy，并建立字段 parity 测试。
- 同时处理 SIGINT/SIGTERM，使用带超时的 `http.Server.Shutdown`。
- 未知 URL 的 metrics path 统一归入 `other`，不产生无限 label cardinality。
- 匿名注册默认关闭；支持 disabled、invite、open 三态，open 必须显式启用。
- 只在配置的 trusted proxy CIDR 后接受 forwarded headers。
- 增加 Argon2 并发 semaphore 和 IP/账号/全局三级限流。
- 非 loopback 明文监听默认拒绝，需显式 unsafe flag。

完成条件：quota 不可通过 mode/allowlist clone 绕过；10,000 个随机 URL 不增加 metrics series。

#### PR-06：SQLite 一致性备份和安全恢复

目标：替换直接打包 live WAL 数据库的方式。

主要改动：

- 在 Go 层提供 SQLite online backup 或等价一致快照能力。
- 备份 staging 包含 DB snapshot、sessions、auth、admin secret、alert state 和 manifest。
- 每个对象带 hash，恢复前验证 archive 路径、hash、schema 和数据库完整性。
- 恢复到 staging，验证后原子切换；禁止损坏归档覆盖现有状态。
- Shell 脚本只作为 Go 备份命令的薄封装，不再自动 fallback 到直接 tar live DB。

完成条件：持续写 WAL 时备份，恢复后 `integrity_check=ok`、`foreign_key_check` 无错误，
表级 count/hash 一致。

### Wave 1：Snapshot 与 Runner 基础

#### PR-07：SnapshotManifest v1

新增 `internal/snapshot`，实现稳定扫描、路径规范化、ignore policy、内容 hash 和
aggregate hash。纯新增，不接入当前候选管道。

测试覆盖：dirty、untracked、add、modify、delete、chmod、空文件、Unicode 路径、
内部/外部 symlink、大文件与扫描上限。

#### PR-08：ChangeSet 和路径策略

实现 add、modify、delete、chmod；新增 allowed/immutable path、二进制、大文件、
symlink 和受保护验收资产策略。

旧 `ExtractedFile` 保留为展示兼容层；遇到无法表达的操作必须报错，不得静默忽略。

#### PR-09：RunnerBackend 契约和停止自动安装

新增 `internal/runner`：

```go
type Capabilities struct {
    ReadOnlyHost bool
    NetworkDeny  bool
    PrivateHome  bool
    ProcessLimit bool
    MemoryLimit  bool
    DiskLimit    bool
}

type ExecSpec struct {
    Argv        []string
    Environment []string
    ReadOnly    []Mount
    Writable    []Mount
    Timeout     time.Duration
    Network     NetworkPolicy
    Limits      ResourceLimits
}

type Backend interface {
    Name() string
    Capabilities(context.Context) (Capabilities, error)
    Run(context.Context, ExecSpec) (Result, error)
}
```

同时：

- 停止候选验证中的自动 dependency install、`go mod tidy`、`pip install --user`。
- 缺依赖返回 `BLOCKED_DEPENDENCY`。
- 将现有 `ExecRestricted` 明确标注为 restricted exec，而非 sandbox。
- safe/autopilot 无满足能力的 Backend 时返回 `ISOLATION_UNAVAILABLE`，零命令启动。

#### PR-10：Linux 隔离 Backend

首版优先使用经过能力探测的 bubblewrap：

- 真实工作区和冻结 Artifact 只读；
- 唯一 writable overlay；
- 私有 HOME、TMP、GOCACHE 等；
- 默认禁网；
- 不挂载 SSH、cloud、npm、registry 等凭据；
- 限制 PID、CPU、内存、磁盘、输出和 wall time；
- 取消后终止整个进程树。

缺少 bwrap、user namespace 或所需资源控制能力时 fail closed，不把不完整的
`unshare` 当作安全替代。

#### PR-11：AgentExecutor 和 Adapter 能力声明

文本 Provider 与修改仓库的 Agent 必须使用不同契约：

```go
type AgentExecutor interface {
    Manifest(context.Context) (AgentManifest, error)
    Execute(context.Context, AttemptRequest) (AttemptResult, error)
}
```

每个 Adapter 声明实际模型、transport、是否能分离 Provider 控制通道与项目工具网络、
是否支持 resume、structured output 和 workspace write。无法证明隔离能力的 Adapter
只能进入 manual。

### Wave 2：可信候选与 Oracle

#### PR-12：冻结完整候选 Artifact

Agent 退出且进程树完成清理后：

1. 完整扫描 Agent 工作区。
2. 生成 CandidateSnapshot 与 ChangeSet。
3. 将内容冻结到内容寻址 Artifact store。
4. Oracle 将 Artifact 只读挂载，并使用一次性 writable overlay。
5. 验证前后重新核对 Artifact hash。

这替代当前“解析模型文件并在第二个 clone 重建”的权威路径。模型文本仍可用于 TUI 展示。

迁移时新旧管道先同时运行 shadow 对比；legacy 管道只能产生 `UNVERIFIED`。

#### PR-13：Go Oracle v1

验证梯级：

```text
0 = 无验证
1 = policy + integrity
2 = format + parse + build + vet
3 = trusted target tests
4 = trusted target + regression checks
```

流程：

- Gate 0：依赖、工具和 baseline preflight。
- Gate 1：路径、验收资产、symlink、二进制、大小和权限策略。
- Gate 2：gofmt、Go parser、build、vet。
- Gate 3：不可变既有测试、用户注册验收命令或隐藏测试。
- Gate 4：全仓回归、coverage 或性能预算。

只有 strength ≥3 才可在 safe/autopilot 下继续。AI judge 不能提高 strength。

#### PR-14：Runstore、repair、approval 和 Apply

- SQLite 保存 run、attempt、snapshot、verification、usage、approval 和状态迁移。
- 默认最多一次 repair，硬上限两次。
- 应用前重扫真实工作区，比较 BaseSnapshot。
- 应用使用 preimage、journal、fsync、临时文件和 rename。
- 启动时恢复或回滚中断的 Apply。
- 所有外部副作用仍需独立授权，不能由 repair 自动重放。

### Wave 3：Router 正确性和预算

#### PR-15：Router instance core

- 将 factory、provider cache、quota、breaker、usage 移入实例 `routerCore`。
- 增加实例级 `RegisterProviderFactory`。
- `internal/model.NewRouter` 注入配置副本，不让闭包引用可变外部对象。
- request view 显式定义共享和私有字段。
- 包级 factory API 保留一版 deprecated shim。

验收：50 个不同配置 Router 并发调用 10 万次，凭据、模型、策略串扰为 0，race 无报告。

#### PR-16：不可变 Policy 与真实 Route 身份

- 将默认表解析成不可变 `RoutingPolicy`。
- Router 持有 policy snapshot/version；单次请求固定使用一个版本。
- 启动时一次性加载并完整验证 override。
- `WatchOverrides` 停止宣传和自动启用，v0.2 deprecated，v0.3 删除。
- Route/Usage/Trace/Audit 记录实际 transport、model、access 和 price key。

#### PR-17：Provider bulkhead 与 request Budget

- 每 Provider semaphore；CLI 默认并发 1，API 可配置。
- half-open 使用 lease/CAS，只允许一个探测请求。
- Budget 贯穿 generator、judge、retry、fallback、repair 和 stream。
- acquire/reserve 必须发生在调用前，取消排队请求及时退出并释放资源。

#### PR-18：Server 原子预算 reservation

新增持久化 `budget_reservations`：

- `request_id` 唯一；
- 整数 micro-USD 作为权威金额；
- SQLite `BEGIN IMMEDIATE` 原子检查 token/org/project 的 settled + reserved；
- 调用完成后与 usage 同事务 settle/refund；
- 崩溃时 reservation fail closed，由有界 TTL/reconciler 回收。

先以 shadow 模式记录拒绝差异，再开启 enforce。

### Wave 4：效果评估

建立 20 个固定 Go bug-fix 任务：

- 15 个公开任务，用于可复现调试；
- 5 个私有 holdout，防止 prompt 或 repair 对测试集过拟合；
- 至少 100 次尝试；
- 对比 direct Claude、Codex、AGY、单 Agent、单 Agent + Oracle、
  单 Agent + Oracle + repair，以及当前 ensemble。

记录：实际模型、transport、policy version、候选列表、随机种子、调用数、token、成本、
延迟、verification evidence、最终人工接受结果。

## 8. 依赖与并行关系

```text
PR-01 -> PR-02 -> PR-03
          |
          +---- PR-05 -> PR-06              # Server 止血线
          |
          +---- PR-15 -> PR-16 -> PR-17 -> PR-18

PR-07 -> PR-08 -> PR-09 -> PR-10
                    |
                    +---- PR-11
                    |
                    +---- PR-12 -> PR-13 -> PR-14 -> Benchmark

PR-04 可在 PR-02 后与 Snapshot、Router、Server 三条线并行。
```

推荐合并批次：

1. Wave 0：PR-01 至 PR-06，恢复可信交付并降低 Server 风险。
2. Wave 1：Snapshot、Runner、Router instance core 三条线并行。
3. Wave 2：冻结候选、Oracle、Linux 隔离和真实身份。
4. Wave 3：Apply、bulkhead、预算 reservation。
5. Wave 4：benchmark，决定是否继续高级编排。

## 9. 当前 dirty worktree 的保护与整理

实施前不得直接把当前所有修改合成一个提交，也不得 reset、checkout 覆盖或丢弃。

建议流程：

1. 记录 `git status --short`、`git diff --stat` 和未跟踪文件清单。
2. 生成 `git diff --binary HEAD` 的只读证据副本。
3. 对未跟踪文件单独归档并生成 SHA-256 manifest。
4. 从当前 `HEAD` 创建独立干净 worktree。
5. 按主题从证据副本恢复路径或 hunk：
   - fallback/API alias 与 model identity；
   - ensemble/stream/usage；
   - CLI/TUI/config；
   - 新增回归测试；
   - MVP 文档。
6. 每个主题分支独立 build/test/vet；不能独立验证的改动继续拆分。
7. 当前工作区在所有主题恢复并核对 hash 前保持不动。

本步骤只定义实施纪律，执行任何 Git 写操作前仍需单独确认。

## 10. CI 分层

| 层级 | 触发 | 目标时限 | 必须执行 |
|---|---|---:|---|
| G0 快速门禁 | 每个 PR | ≤8 分钟 | gofmt、build、`go test ./...`、`go vet ./...` |
| G1 完整 PR | 每个 PR/Linux | ≤20 分钟 | race、Snapshot/Apply、Router、预算、SQLite、sandbox 集成 |
| G2 故障与压力 | Nightly | ≤90 分钟 | fuzz、并发 `-count=100`、kill、ENOSPC、SQL busy、WAL 热备份、逃逸测试、benchmark |
| G3 发布契约 | Tag 前与 Tag workflow | ≤45 分钟 | 三平台 artifact、安装器、容器、升级恢复、签名、SBOM、provenance |
| G4 模型效果 | Weekly/候选版本 | 长任务 | 固定任务集与真实 Provider，对 safe/autopilot 和高级编排做晋级判断 |

测试失败不能通过无条件重跑掩盖。随机测试必须记录 seed；nightly 同一 seed 连续两次失败
升级为发布阻断缺陷。

## 11. 关键验收矩阵

### 11.1 Snapshot 与 Apply

| 场景 | 门槛 |
|---|---|
| tracked dirty、untracked、增删改、chmod、Unicode、空文件 | 变更发现率 100%，漏报/误报为 0 |
| 同一目录随机遍历 100 次 | manifest 字节和 hash 完全一致 |
| base + ChangeSet | 与 CandidateSnapshot hash 完全一致 |
| Snapshot 后篡改候选 | 返回 `STALE_CANDIDATE`，零真实工作区写入 |
| 应用前用户修改工作区 | 返回 `STALE_WORKSPACE`，保留用户修改 |
| 第 N 次写入注入 EACCES/ENOSPC/kill | 恢复后为完整旧状态或完整新状态，不出现混合状态 |

### 11.2 Oracle

| 场景 | 门槛 |
|---|---|
| 已知正确 patch | 通过率 100% |
| seeded 故障 patch | 拒绝率 ≥95% |
| 修改既有测试、CI、verifier、权限 | 误接收数 0 |
| 仅新增自证测试 | 不能达到 strength 3 |
| baseline 已失败或缺依赖 | 明确分类，不计为 Agent 质量失败 |

### 11.3 安全隔离

| 攻击 fixture | 门槛 |
|---|---|
| 读取真实 HOME、SSH、cloud、npm、API key | secret exposure 为 0 |
| 写真实工作区、HOME 或其他宿主路径 | unauthorized write 为 0 |
| TCP/UDP/DNS/IPv6/代理逃逸 | 项目工具成功外连数为 0 |
| fork bomb、无限内存/磁盘/输出 | 在配置上限终止，2 秒内无残留后代 |
| 缺少隔离能力 | safe/autopilot 零命令启动，必须 fail closed |

### 11.4 Router

| 场景 | 门槛 |
|---|---|
| 50 个不同 key/policy Router 并发调用 | credential/model/policy 串扰为 0 |
| mode/allowlist request view | quota seal 和 policy 保留率 100% |
| 1000 个 goroutine 同时进入 half-open | 恰好 1 个 Provider 探测请求 |
| CLI/API alias、fallback、stream、ensemble | Route、Usage、Audit、Cost 的实际身份 100% 一致 |
| 10 万次取消/reload/stream | goroutine 净增长 ≤5，无 permit 泄漏 |

### 11.5 预算与账本

| 场景 | 门槛 |
|---|---|
| $1 预算、1000 个并发 $0.01 reservation | 最多 100 个成功，超支 ≤1 micro-USD |
| retry/ensemble/stream 中断 | 已发生调用全部结算，未发生调用全部 refund |
| 重复回调和 request retry | usage/audit/settlement 恰好一次 |
| SQL busy、disk full | 不静默成功，产生稳定错误和告警 |

### 11.6 备份与恢复

| 场景 | 门槛 |
|---|---|
| WAL 持续写入时在线备份 | barrier 前已提交数据全部恢复 |
| 损坏、截断、路径穿越归档 | 写目标前失败，原目标 hash 不变 |
| N-1/N-2 schema 升级 | 数据和预算账本不丢，迁移可重试 |
| SIGTERM 时有 stream/transaction | 立即 not-ready，30 秒内 drain，已确认写入不丢 |

## 12. 性能与质量指标

- 总语句覆盖率：短期 ≥60%，随后 ≥65%。
- `router`、`internal/engine`、`serverauth`、`serveradmin`：≥70%。
- `serverdb`：≥80%。
- 新增生产函数圈复杂度 ≤20；热点函数逐步拆至 ≤80 行。
- 10k 文件、40 MiB fixture：首屏 p95 <300ms，内存 <100 MiB。
- Candidate writable layer 准备 p95 <1s，无变化 diff <300ms。
- 每候选最多创建一个 Agent 工作区和一个验证 overlay，不再进行最多六次全仓复制。
- 控制面开销不超过任务总耗时 5%。
- TUI update p95 <16ms，键盘响应 p95 <50ms。
- 完成的会话 turn 在 2 秒内原子保存，崩溃后不丢已完成 turn。

## 13. 发布计划

### 13.1 v0.2.0-beta.1

范围：Wave 0、Snapshot/ChangeSet schema。

发布门槛：

- 全仓 build/test/vet/race 通过，所有正式包入闸。
- latest artifact、版本、README、CHANGELOG 和命令清单一致。
- Server 明确 alpha，安全远程文档修复。
- quota clone、SIGTERM、注册默认关闭、metrics 有界、在线备份完成。
- Snapshot/ChangeSet 仅 shadow/off，不自动应用。

默认关闭：

- Server 默认用户路径；
- 匿名注册；
- safe/autopilot；
- 自动依赖下载；
- 新候选管道自动应用。

### 13.2 v0.2.0-beta.2

范围：Linux Runner、冻结 Artifact、Oracle、Apply、Router 实例化。

发布门槛：

- Linux 隔离逃逸 fixture 全部通过。
- 新旧候选管道 shadow 对比无已知漏变更。
- Snapshot/Oracle/Apply 故障注入通过。
- Router 多实例隔离 `-race -count=100` 通过。
- macOS/Windows 的 manual + unsafe consent 行为有 E2E 测试。

默认关闭：

- Server GA；
- Planner/DAG/多写 Agent；
- 非 Linux autopilot；
- 原子预算 enforce，先 shadow。

### 13.3 v0.2.0

只有以下条件全部满足才发布稳定版：

- 新 Snapshot 管道默认开启，legacy 只能返回 `UNVERIFIED`。
- safe/autopilot 只在能力满足的 Linux 环境开放。
- 完成 20 个任务、至少 100 次客观评估。
- verified completion 相对最强 direct Agent 非劣不超过 2 个百分点。
- 一次 repair 产生可测量的正恢复率。
- Oracle false-accept 和全部安全硬门禁达标。
- 三平台 artifact smoke、安装、升级和恢复通过。

Server 仍为 alpha，不随 CLI v0.2.0 获得 GA 承诺。

### 13.4 Server GA

另行评审，至少要求：

- 原子持久预算、可靠 usage/audit、schema migration 和备份恢复演练；
- TLS 或经过验证的反向代理部署模板；
- 注册、登录、trusted proxy 和资源耗尽防护；
- readiness/liveness、Provider/SQLite/webhook 指标和告警；
- N-1 升级、回滚与 RTO/RPO 记录；
- 连续三个版本通过 Server 发布契约。

## 14. 暂缓、弃用与禁止事项

### 暂缓

- Planner、静态 DAG 和并行写 Agent。
- 更复杂的 quota forecasting/pacing。
- 多语言 Oracle。
- Server HA、多节点和公网 GA。
- 非 Linux autopilot。
- 开放匿名注册。
- AI judge 直接影响机器验证强度。

### 弃用或删除

- `ExtractedFile` 作为权威变更协议。
- 候选验证阶段的自动依赖安装。
- CLI routing hot reload；v0.2 deprecated，v0.3 删除。
- 每请求重写全量 JSON routing stats。
- 包级可变 factory 和策略表；兼容 shim 保留一版后删除。
- 静默 sandbox 降级。
- 直接归档 live WAL 数据库。

### 明确禁止

- Agent 修改权限、预算、verifier 或停止条件。
- 未经批准的 Git commit/push、PR、部署和数据库迁移。
- 缺少隔离能力时继续标记 Verified。
- 把 AI reviewer 的判断当作机器验证证据。
- 为消耗即将过期的订阅 quota 而执行低价值任务。

## 15. 仍需维护者确认的事项与默认建议

| 事项 | 默认建议 |
|---|---|
| Server 命令是否从默认帮助隐藏 | 先归入 experimental 分组并显示 alpha；避免已有脚本突然找不到命令 |
| Linux 隔离实现 | 使用 bubblewrap 并做能力探测；不自研 namespace 安全边界 |
| Candidate 验证位置 | 冻结 Artifact 只读挂载 + disposable overlay，不在 Agent 工作目录原地强验证 |
| 公开 API shim 删除版本 | v0.2 deprecated，v0.3 删除 |
| Benchmark 任务治理 | 15 个公开 + 5 个维护者持有的 holdout |
| Server 与 CLI 是否拆 module/repo | 本轮不拆；v0.3 完成包迁移后再评估 |
| macOS/Windows 自动执行 | 默认 manual + 显式 unsafe consent，直到有等价隔离后端 |

## 16. Definition of Done

本轮优化完成的定义：

1. 主干、latest Release、README 和安装器描述同一个产品。
2. 所有正式包进入 build/test/vet/race 门禁。
3. Makewand 能完整捕获 Agent 工作区的每个受支持变更。
4. Oracle 验证的冻结 Snapshot 与待应用 Snapshot 完全一致。
5. safe/autopilot 在缺少强隔离能力时 fail closed。
6. Apply 能检测 stale，并在故障后恢复到一致状态。
7. 多 Router 实例之间不存在 factory、凭据、策略和 quota 串扰。
8. 并发请求不能穿透调用或金额预算。
9. Server 可以一致备份、验证恢复并优雅退出，但仍诚实标记 alpha。
10. 固定 benchmark 已证明单 Agent + Oracle 的价值，或明确决定停止高级编排投资。

在以上条件满足前，不扩大自动执行权限，不推进 Planner 和多写 Agent。
