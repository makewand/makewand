# Makewand (魔杖) v3.1.0

> **多模型编程订阅联合调度与红队自愈体系** (Unified Multi-Model Subscription Orchestrator)  
> 官方网站：[https://makewand.org](https://makewand.org) · 备用镜像：[https://makewand.com](https://makewand.com)  
> 统合调用本机 **Antigravity (Google AI Pro)**、**Claude Code**、**Codex CLI (OpenAI)**、**Grok Build**、**Muse Code** 与全生态主流编程模型（15+ 工具动态拓扑感知），实现零额外 Token 成本、跨模型红队盲审、自愈回环与并发竞速。

```bash
# 🚀 官方一键快速安装
curl -fsSL https://makewand.org/install.sh | bash
```

---

## 🌟 核心特性 (Key Features)

- **🌐 官方网站与文档中心**：托管于 Cloudflare Pages 全球边缘网络（[makewand.org](https://makewand.org) / [makewand.com](https://makewand.com)），提供交互式终端模拟器与完整手册。
- **🔌 全生态工具自适应拓扑**：动态感知用户已登录的工具池（用户配置几个就使用几个，N>=2 启动异构交叉互审，N=1 启动 Shadow Worktree 独立批判自省）。
- **💰 零额外 Token 计费**：完全基于本机已订阅的官方 CLI 工具（`agy`、`claude`、`codex`、`muse`、`grok`），不产生第三方 API 扣费。
- **⚡ 额度感知与自适应降级 (Quota-Aware Routing)**：
  - 自动检测各订阅的 5小时/每周额度与限流状态，精准识别解封重置时间。
  - 当主力模型（如 Claude）达到使用上限时，秒级自动降级至备用健康模型（Codex / Muse / Antigravity），确保任务不中断。
- **🛡️ 跨模型独立红队审查 (Cross-Model Review)**：
  - 严格恪守“实现者与审查者物理隔离”原则，杜绝自审盲区。
  - Codex (gpt-6-astra) 深入字节码与并发模型；Antigravity (Gemini 3.8 Pro) 提供百万级全局架构分析。
- **🔄 自愈修复闭环 (Auto-Fix Loop)**：
  - 审查若发现 `[P1]`（高危）、`[P2]`（中危）、死锁或竞态缺陷，自动提取反馈重派编码方修复，并持续复审直至 `LGTM`。
- **🏁 双模型并发隔离竞速 (Race Mode)**：
  - `makewand race "<任务>"` 在临时隔离沙箱中并行派发两组模型，统计耗时与代码质量，由 Antigravity 担任主裁判输出对比矩阵与采纳裁决。
- **🌲 非 Git 目录弹性容灾**：未初始化 Git 的工程自动构建轻量级影子跟踪树，彻底解决常规工具依赖 Git 导致的崩溃。
- **🚀 无头执行与权限自动穿透**：注入 `--dangerously-skip-permissions` 与 `--dangerously-bypass-approvals-and-sandbox`，无弹窗打断，全程无人值守闭环。
- **📡 实时流式输出**：支持 `--stream` 参数，带模型专属色彩前缀逐行流式呈现。

---

## 🏗️ 架构拓扑 (Architecture)

```mermaid
graph TD
    User([终端用户 / AI Agent]) --> MakewandCLI[Makewand CLI / Skill]
    
    subgraph 决策与探活层
        MakewandCLI --> Health[健康与配额感知器 health.py]
        Health --> Cache[~/.config/makewand/status.json]
        MakewandCLI --> Router[自适应路由与档位判定]
    end

    subgraph 订阅模型池 (本地已登录 CLI)
        Router --> AGY[Antigravity CLI\nGoogle AI Pro / Gemini 3.8]
        Router --> Claude[Claude Code CLI\nAnthropic 订阅]
        Router --> Codex[Codex CLI\nOpenAI / gpt-6-astra]
        Router --> Muse[Muse Code CLI\nMeta 订阅 / Llama]
    end

    subgraph 协同流水线 Pipeline
        Claude -->|主力实现| Diff[代码 Diff 生成]
        Codex -->|降级实现| Diff
        Diff -->|独立红队审查| CodexReview[Codex / AGY 深度审查]
        CodexReview -->|检测到 P1/P2 缺陷| AutoFix[Auto-Fix 修复循环]
        AutoFix --> Diff
        CodexReview -->|审核通过 LGTM| Done[最终交付验收]
    end
```

---

## 📦 安装与配置 (Installation)

### 快速安装
```bash
cd path/to/makewand
./scripts/install.sh
```
此命令会将可执行文件安装至 `~/.local/bin/makewand`，并建立向后兼容的 `trio` 软链接。

---

## 💻 命令行用法速查 (CLI Reference)

### 1. 订阅健康状态与额度看板
```bash
# 查看四大模型实时可用状态与限流时间
makewand status
makewand quota

# 强制即刻发起实时探活
makewand probe
```

### 2. 动态模型发现
```bash
# 自动探测系统配置文件中记录的最新模型版本
makewand models
```

### 3. 全自动联合编码流水线
```bash
# 执行端到端实现 + 跨模型审计 + 自愈回环
makewand run "在当前目录编写一个支持重试与指数退避的 HTTP 请求客户端"

# 指定工作目录并启用实时流式输出
makewand run "重构用户认证模块" --cwd /path/to/repo --stream

# 指定推理档位 (fast / standard / deep)
makewand run "排查并发线程安全" --tier deep
```

### 4. 独立代码审查
```bash
# 针对当前目录未提交的 git diff 实施红队安全审查
makewand review
```

### 5. 双模型并发竞速模式
```bash
# 两位选手并行实现，由 Antigravity 输出对比矩阵并裁决
makewand race "实现一个快速排序函数 quick_sort(arr) 并写测试用例验证"
```

### 6. 带预算安全搜索 (避免冷归档与大库 I/O 卡死)
```bash
makewand search "def safe_search" --cwd /path/to/project
```

### 7. Bubblewrap 物理进程沙箱隔离执行
```bash
# 隔离运行单元测试 (只读根目录、凭据自动脱敏、仅工作区可写)
makewand sandbox python3 -m pytest tests/

# 阻断外网访问执行构建/测试
makewand sandbox --no-net go test ./...
```

### 8. 单模型直接透传调用
```bash
makewand agy "总结当前项目的架构优势"
makewand codex "分析这段代码的死锁风险"
makewand claude "编写测试用例"
makewand muse "检查系统配置项"
```

---

## 🤖 主流 AI 协同接入 (AI Integrations)

Makewand 原生设计为不仅面向人类终端开发者，更作为所有 AI 代理的“外挂魔杖”：
- **Antigravity (AGY)**：内置 `makewand-orchestrator` Skill，在处理复杂架构任务时自主调用。
- **Claude Code**：在 `CLAUDE.md` 中指引调用 `makewand review` 实现外部交叉审计。
- **Codex CLI**：在 `default.rules` 中支持调度 `makewand race` 与 `makewand run`。
- **Muse Code**：支持在 Meta 生态中联动调度其它模型。

详细指引见 [integrations/](integrations/) 目录。

---

## 🧪 自动化测试 (Testing)

```bash
make test
# 或
python3 -m unittest discover tests
```
