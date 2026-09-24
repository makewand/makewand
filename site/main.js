/**
 * Makewand Official Website Interactive Logic
 */

document.addEventListener("DOMContentLoaded", () => {
  initTerminalTabs();
  initCopyButtons();
  initEcosystemFilters();
});

// Terminal Tabs Data & Simulator
const terminalScenarios = {
  run: `
<span class="term-prompt">user@dev:~/project$</span> <span class="term-cmd">makewand run "Refactor token auth to support ed25519 signatures"</span>
<span class="term-info">🔍 [Makewand Dynamic Topology] Probing local environment tools...</span>
<span class="term-success">✓ Active provider pool (N=5): [claude, codex, agy, grok, muse]</span>
<span class="term-info">⚡ Topology selected: Heterogeneous Cross-Model Review Mode</span>
<span class="term-sub">--------------------------------------------------------------------------------</span>
<span class="term-prompt">🔨 [Phase 1: Implementation]</span> Primary coder: <span class="term-info">Claude Code (Claude 3.7 Sonnet)</span>
  ↳ Created isolated worktree: <span class="term-sub">.makewand_sandbox/task-ed25519-auth</span>
  ↳ Implemented ed25519 signature parser & unit test suite (3 files changed, +148 lines)
  ↳ Running local test suite: <span class="term-success">18 passed, 0 failed in 0.42s</span>
<span class="term-prompt">🛡️ [Phase 2: Red-Team Blind Review]</span> Adversarial auditor: <span class="term-info">Codex CLI (gpt-6-astra)</span>
  ↳ Reviewing diff without implementation prompt bias...
  ↳ <span class="term-warn">⚠️ Finding [HIGH]: Key rotation lacks monotonic timestamp nonce verification</span>
<span class="term-prompt">🔄 [Phase 3: Auto-Fix Loop]</span> Handing feedback back to primary coder...
  ↳ Claude applied replay-attack protection with nonce cache.
  ↳ Re-running test suite: <span class="term-success">21 passed, 0 failed in 0.48s</span>
<span class="term-success">✨ [Completed] Task successfully closed with zero human intervention in 14.2s!</span>
`,

  review: `
<span class="term-prompt">user@dev:~/project$</span> <span class="term-cmd">makewand review --provider codex --target src/payment_gateway.py</span>
<span class="term-info">🔍 [Makewand Blind Audit] Spawning isolated worktree for zero-bias security review...</span>
<span class="term-sub">Running static AST analysis & edge-case fuzzing with gpt-6-astra...</span>
<span class="term-sub">--------------------------------------------------------------------------------</span>
<span class="term-warn">● [AUDIT FINDING 1] Potential Float Precision Drift (Financial Invariant)</span>
  File: <span class="term-sub">src/payment_gateway.py:142</span>
  Detail: Transaction fee is calculated using standard float division instead of Decimal.
  Suggested Fix: Use <span class="term-success">from decimal import Decimal</span> with quantize ROUND_HALF_UP.

<span class="term-warn">● [AUDIT FINDING 2] Unbounded Retry Backoff in Webhook Delivery</span>
  File: <span class="term-sub">src/payment_gateway.py:289</span>
  Detail: Exponential backoff without jitter may trigger thundering herd on gateway recovery.

<span class="term-info">💡 Auto-Fix Recommendation: Run 'makewand apply --fix' to auto-patch all 2 findings.</span>
`,

  race: `
<span class="term-prompt">user@dev:~/project$</span> <span class="term-cmd">makewand race "Implement parallel zarr tensor slice loader" --models claude,codex</span>
<span class="term-info">🏁 [Makewand Race Engine] Dual-Model Concurrent Speed & Quality Race</span>
<span class="term-sub">Creating 2 parallel sandboxed worktrees...</span>
  ↳ Worktree A: <span class="term-sub">.makewand_sandbox/race_claude</span>
  ↳ Worktree B: <span class="term-sub">.makewand_sandbox/race_codex</span>
<span class="term-info">🚀 Both models dispatched concurrently at 0 token API cost (using local subscriptions)...</span>
  [Claude] Finished in 9.2s · Tests: 12/12 passed · Benchmark: 1.42 GB/s
  [Codex]  Finished in 11.5s · Tests: 12/12 passed · Benchmark: 1.89 GB/s
<span class="term-success">🏆 Winner: Codex (Higher throughput: +33% I/O performance)</span>
<span class="term-info">Applying winning candidate to main working tree... Done!</span>
`,

  observe: `
<span class="term-prompt">user@dev:~/project$</span> <span class="term-cmd">makewand observe</span>
<span class="term-info">### 🕒 Makewand 跨会话运行态势与分型巡检汇报 (2026-09-24 11:55:33)</span>
<span class="term-sub">系统资源负荷：1m 负载: 18.79 | 内存可用: 61.2 GB / 125.5 GB | GPU: RTX 5880 Ada (100% Util, 38.2 GB)</span>

| 会话名称 | 工作区路径 | 操作分型 | 运行态势与细节 | 状态 |
|---|---|---|---|:---:|
| <span class="term-cmd">adlims</span> | /mnt/data/adlims | test_ci | PR #852 CI 全绿，已自动合并完成 | <span class="term-success">🟢 闭环就绪</span> |
| <span class="term-cmd">stock</span> | /mnt/data/stock | server_daemon | 零售仪表盘后台服务正常监听 (8765) | <span class="term-success">🟢 运行中</span> |
| <span class="term-cmd">whereifish</span> | /mnt/data/whereifish | quota_exhausted | 模型配额已见底 (0% left)，AGY接管 | <span class="term-warn">⚠️ 额度已见底</span> |
| <span class="term-cmd">watersmap</span> | /mnt/data/watersmap | test_ci | 单元与端到端自动化测试进行中 | <span class="term-info">🔵 测试中</span> |
| <span class="term-cmd">makewand</span> | /mnt/data/makewand | idle_ready | v3.1.0 已发布，工作区干净待命 | <span class="term-success">🟢 就绪空闲</span> |
`
};

function initTerminalTabs() {
  const tabs = document.querySelectorAll(".terminal-tab");
  const body = document.getElementById("terminalBody");
  if (!body) return;

  tabs.forEach(tab => {
    tab.addEventListener("click", () => {
      tabs.forEach(t => t.classList.remove("active"));
      tab.classList.add("active");
      const key = tab.getAttribute("data-tab");
      if (terminalScenarios[key]) {
        body.innerHTML = terminalScenarios[key].trim();
      }
    });
  });

  // Default load
  body.innerHTML = terminalScenarios.run.trim();
}

function initCopyButtons() {
  const copyBtns = document.querySelectorAll(".copy-btn");
  copyBtns.forEach(btn => {
    btn.addEventListener("click", () => {
      const textToCopy = btn.getAttribute("data-copy") || btn.parentElement.innerText.trim();
      navigator.clipboard.writeText(textToCopy).then(() => {
        const origText = btn.innerHTML;
        btn.innerHTML = `<span style="color:#10b981;font-size:0.8rem;">已复制 ✓</span>`;
        setTimeout(() => {
          btn.innerHTML = origText;
        }, 2000);
      });
    });
  });
}

function initEcosystemFilters() {
  const buttons = document.querySelectorAll(".filter-btn");
  const cards = document.querySelectorAll(".tool-card");

  buttons.forEach(btn => {
    btn.addEventListener("click", () => {
      buttons.forEach(b => b.classList.remove("active"));
      btn.classList.add("active");

      const filter = btn.getAttribute("data-filter");
      cards.forEach(card => {
        const cat = card.getAttribute("data-category");
        if (filter === "all" || cat === filter) {
          card.style.display = "flex";
        } else {
          card.style.display = "none";
        }
      });
    });
  });
}
