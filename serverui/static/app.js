const state = {
  token: "",
  authMode: "",
  csrfToken: "",
  userEmail: "",
};

try {
  [
    "makewand_admin_token",
    "makewand_admin_auth_mode",
    "makewand_admin_csrf",
    "makewand_admin_user_email",
  ].forEach((key) => sessionStorage.removeItem(key));
} catch {
  // Session storage can be unavailable in hardened browser contexts.
}

const nodes = {
  loginCard: document.getElementById("login-card"),
  appShell: document.getElementById("app-shell"),
  sessionStatus: document.getElementById("session-status"),
  loginError: document.getElementById("login-error"),
  metricRequests: document.getElementById("metric-requests"),
  metricSpend: document.getElementById("metric-spend"),
  metricTokens: document.getElementById("metric-tokens"),
  metricProjects: document.getElementById("metric-projects"),
  serviceSummary: document.getElementById("service-summary"),
  usageSummary: document.getElementById("usage-summary"),
  usersTable: document.getElementById("users-table"),
  tokensTable: document.getElementById("tokens-table"),
  organizationsTable: document.getElementById("organizations-table"),
  projectsTable: document.getElementById("projects-table"),
  orgMembershipsTable: document.getElementById("org-memberships-table"),
  projectMembershipsTable: document.getElementById("project-memberships-table"),
  billingOrgs: document.getElementById("billing-orgs"),
  billingProjects: document.getElementById("billing-projects"),
  billingAlerts: document.getElementById("billing-alerts"),
  billingPeriods: document.getElementById("billing-periods"),
  tokenIssueResult: document.getElementById("token-issue-result"),
  chatHistory: document.getElementById("chat-history-container"),
  playgroundForm: document.getElementById("playground-form"),
  playgroundInput: document.getElementById("playground-input"),
  playgroundSendBtn: document.getElementById("playground-send-btn"),
  chatIntentBadge: document.getElementById("chat-intent-badge"),
  chatClearBtn: document.getElementById("chat-clear-btn"),
};

function isMutation(method) {
  switch ((method || "GET").toUpperCase()) {
    case "POST":
    case "PUT":
    case "PATCH":
    case "DELETE":
      return true;
    default:
      return false;
  }
}

async function api(path, options = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = new Headers(options.headers || {});
  if (state.token) {
    headers.set("Authorization", `Bearer ${state.token}`);
  }
  if (options.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  if (!state.token && state.csrfToken && isMutation(method)) {
    headers.set("X-CSRF-Token", state.csrfToken);
  }
  const response = await fetch(path, {
    ...options,
    method,
    headers,
    credentials: "same-origin",
  });
  const text = await response.text();
  let data = {};
  if (text.trim()) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { raw: text };
    }
  }
  if (!response.ok) {
    throw new Error(data?.error?.message || data.raw || response.statusText);
  }
  return data;
}

function setSession({ token = "", authMode = "", csrfToken = "", userEmail = "" } = {}) {
  state.token = token;
  state.authMode = authMode;
  state.csrfToken = csrfToken;
  state.userEmail = userEmail;

  if (state.token || state.authMode === "session") {
    nodes.sessionStatus.textContent = state.userEmail ? `Signed in as ${state.userEmail}` : "Signed in";
  } else {
    nodes.sessionStatus.textContent = "Signed out";
  }
}

function clearSession() {
  setSession({});
}

function showApp(visible) {
  nodes.loginCard.classList.toggle("hidden", visible);
  nodes.appShell.classList.toggle("hidden", !visible);
}

function clearNode(target) {
  while (target.firstChild) {
    target.removeChild(target.firstChild);
  }
}

function appendCellContent(cellNode, value) {
  if (value instanceof Node) {
    cellNode.appendChild(value);
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((item, index) => {
      if (index > 0) {
        cellNode.appendChild(document.createTextNode(" "));
      }
      appendCellContent(cellNode, item);
    });
    return;
  }
  cellNode.textContent = value == null ? "" : String(value);
}

function actionButton(label, dataset = {}) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "ghost-button";
  button.textContent = label;
  Object.entries(dataset).forEach(([key, value]) => {
    button.dataset[key] = String(value);
  });
  return button;
}

function renderTable(target, headers, rows) {
  clearNode(target);
  if (!rows.length) {
    const empty = document.createElement("p");
    empty.className = "hint-text";
    empty.textContent = "No data yet.";
    target.appendChild(empty);
    return;
  }
  const table = document.createElement("table");
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  headers.forEach((header) => {
    const th = document.createElement("th");
    th.textContent = header;
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");
  rows.forEach((row) => {
    const tr = document.createElement("tr");
    row.forEach((cell) => {
      const td = document.createElement("td");
      appendCellContent(td, cell);
      tr.appendChild(td);
    });
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  target.appendChild(table);
}

function money(value) {
  return `$${Number(value || 0).toFixed(2)}`;
}

function percent(value) {
  return `${Number(value || 0).toFixed(2)}%`;
}

async function refresh() {
  const [dashboard, users, tokens, orgs, projects, orgMemberships, projectMemberships, billing, billingAlerts, billingPeriods] = await Promise.all([
    api("/v1/admin/dashboard"),
    api("/v1/admin/users?limit=100"),
    api("/v1/admin/tokens?limit=100"),
    api("/v1/admin/organizations?limit=100").catch(() => ({ data: [] })),
    api("/v1/admin/projects?limit=100").catch(() => ({ data: [] })),
    api("/v1/admin/organization-memberships?limit=100").catch(() => ({ data: [] })),
    api("/v1/admin/project-memberships?limit=100").catch(() => ({ data: [] })),
    api("/v1/admin/billing/summary").catch(() => ({ billing: { organizations: [], projects: [] }, usage: {} })),
    api("/v1/admin/billing/alerts").catch(() => ({ alerts: [] })),
    api("/v1/admin/billing/periods").catch(() => ({ periods: [] })),
  ]);

  const usage = dashboard.usage?.summary || {};
  nodes.metricRequests.textContent = usage.total_requests || 0;
  nodes.metricSpend.textContent = money(usage.total_cost_usd || 0);
  nodes.metricTokens.textContent = dashboard.tokens?.count || 0;
  nodes.metricProjects.textContent = dashboard.projects?.count || 0;

  nodes.serviceSummary.textContent = JSON.stringify({
    tokens: dashboard.tokens?.count || 0,
    users: dashboard.users?.count || 0,
    organizations: dashboard.organizations?.count || 0,
    projects: dashboard.projects?.count || 0,
  }, null, 2);
  nodes.usageSummary.textContent = JSON.stringify(usage, null, 2);

  renderTable(nodes.usersTable, ["ID", "Email", "Role", "Active", "Actions"], users.data.map((user) => [
    user.id,
    user.email,
    user.role,
    user.is_active ? "yes" : "no",
    [
      actionButton(user.role === "admin" ? "Make member" : "Promote admin", {
        userRole: user.id,
        nextRole: user.role === "admin" ? "member" : "admin",
      }),
      actionButton(user.is_active ? "Deactivate" : "Activate", {
        userActive: user.id,
        nextActive: user.is_active ? "false" : "true",
      }),
    ],
  ]));

  renderTable(nodes.tokensTable, ["ID", "Description", "User", "Org", "Project", "Scopes", "Revoked", "Actions"], tokens.data.map((token) => [
    token.id,
    token.description || "",
    token.user_id || "",
    token.organization_id || "",
    token.project_id || "",
    (token.scopes || []).join(", "),
    token.revoked ? "yes" : "no",
    token.revoked ? "revoked" : actionButton("Revoke", { tokenRevoke: token.id }),
  ]));

  renderTable(nodes.organizationsTable, ["ID", "Name", "Budget"], orgs.data.map((org) => [
    org.id,
    org.name,
    money(org.monthly_budget_usd),
  ]));

  renderTable(nodes.projectsTable, ["ID", "Org", "Name", "Budget"], projects.data.map((project) => [
    project.id,
    project.organization_id,
    project.name,
    money(project.monthly_budget_usd),
  ]));

  renderTable(nodes.orgMembershipsTable, ["Org", "User", "Role", "Active"], orgMemberships.data.map((membership) => [
    membership.organization_id,
    membership.user_id,
    membership.role,
    membership.is_active ? "yes" : "no",
  ]));

  renderTable(nodes.projectMembershipsTable, ["Project", "Org", "User", "Role", "Active"], projectMemberships.data.map((membership) => [
    membership.project_id,
    membership.organization_id,
    membership.user_id,
    membership.role,
    membership.is_active ? "yes" : "no",
  ]));

  renderTable(nodes.billingOrgs, ["Organization", "Budget", "Spend", "Remaining", "Utilization", "Status", "Requests"], (billing.billing?.organizations || []).map((bucket) => [
    bucket.name,
    money(bucket.monthly_budget_usd),
    money(bucket.spend_usd),
    money(bucket.remaining_budget_usd),
    percent(bucket.utilization_percent),
    bucket.over_budget ? "over budget" : "ok",
    bucket.request_count || 0,
  ]));

  renderTable(nodes.billingProjects, ["Project", "Budget", "Spend", "Remaining", "Utilization", "Status", "Requests"], (billing.billing?.projects || []).map((bucket) => [
    bucket.name,
    money(bucket.monthly_budget_usd),
    money(bucket.spend_usd),
    money(bucket.remaining_budget_usd),
    percent(bucket.utilization_percent),
    bucket.over_budget ? "over budget" : "ok",
    bucket.request_count || 0,
  ]));

  renderTable(nodes.billingAlerts, ["Scope", "Name", "Severity", "Budget", "Spend", "Remaining", "Utilization"], (billingAlerts.alerts || []).map((alert) => [
    alert.scope_type,
    alert.name,
    alert.severity,
    money(alert.monthly_budget_usd),
    money(alert.spend_usd),
    money(alert.remaining_budget_usd),
    percent(alert.utilization_percent),
  ]));

  renderTable(nodes.billingPeriods, ["Period", "Requests", "Prompt", "Completion", "Cost"], (billingPeriods.periods || []).map((period) => [
    period.period,
    period.total_requests || 0,
    period.total_prompt_tokens || 0,
    period.total_completion_tokens || 0,
    money(period.total_cost_usd),
  ]));
}

async function loginWithEmailPassword(email, password) {
  const response = await fetch("/v1/admin/session/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ email, password }),
  });
  const payload = await response.json();
  if (!response.ok) {
    throw new Error(payload?.error?.message || response.statusText);
  }
  setSession({
    authMode: "session",
    csrfToken: payload.csrf_token || "",
    userEmail: payload.user?.email || "",
  });
}

async function restoreSession() {
  if (state.token) {
    setSession({
      token: state.token,
      authMode: "bearer",
      csrfToken: "",
      userEmail: state.userEmail,
    });
    return true;
  }
  try {
    const payload = await api("/v1/admin/session/me");
    if (!payload.authenticated) {
      clearSession();
      return false;
    }
    setSession({
      authMode: "session",
      csrfToken: payload.csrf_token || "",
      userEmail: payload.user?.email || "",
    });
    return true;
  } catch {
    clearSession();
    return false;
  }
}

document.getElementById("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  nodes.loginError.textContent = "";
  try {
    await loginWithEmailPassword(
      document.getElementById("login-email").value,
      document.getElementById("login-password").value,
    );
    showApp(true);
    await refresh();
  } catch (error) {
    nodes.loginError.textContent = error.message;
  }
});

document.getElementById("token-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  nodes.loginError.textContent = "";
  try {
    setSession({
      token: document.getElementById("token-value").value.trim(),
      authMode: "bearer",
      csrfToken: "",
      userEmail: "",
    });
    showApp(true);
    await refresh();
  } catch (error) {
    clearSession();
    showApp(false);
    nodes.loginError.textContent = error.message;
  }
});

document.getElementById("issue-token-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const payload = {
    description: document.getElementById("token-description").value.trim(),
    user_id: document.getElementById("token-user-id").value.trim(),
    organization_id: document.getElementById("token-org-id").value.trim(),
    project_id: document.getElementById("token-project-id").value.trim(),
    scopes: document.getElementById("token-scopes").value.split(",").map((item) => item.trim()).filter(Boolean),
    allowed_providers: document.getElementById("token-providers").value.split(",").map((item) => item.trim()).filter(Boolean),
    allowed_modes: document.getElementById("token-modes").value.split(",").map((item) => item.trim()).filter(Boolean),
    max_requests_per_day: Number(document.getElementById("token-max-requests-day").value || 0),
    max_cost_usd_per_month: Number(document.getElementById("token-max-cost-month").value || 0),
  };
  try {
    const result = await api("/v1/admin/tokens", { method: "POST", body: JSON.stringify(payload) });
    nodes.tokenIssueResult.textContent = `Issued ${result.token_id}: ${result.token}`;
    await refresh();
  } catch (error) {
    nodes.tokenIssueResult.textContent = error.message;
  }
});

document.getElementById("org-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  await api("/v1/admin/organizations", {
    method: "POST",
    body: JSON.stringify({
      name: document.getElementById("org-name").value.trim(),
      slug: document.getElementById("org-slug").value.trim(),
      description: document.getElementById("org-description").value.trim(),
      monthly_budget_usd: Number(document.getElementById("org-budget").value || 0),
    }),
  });
  event.target.reset();
  await refresh();
});

document.getElementById("project-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  await api("/v1/admin/projects", {
    method: "POST",
    body: JSON.stringify({
      organization_id: document.getElementById("project-org-id").value.trim(),
      name: document.getElementById("project-name").value.trim(),
      slug: document.getElementById("project-slug").value.trim(),
      description: document.getElementById("project-description").value.trim(),
      monthly_budget_usd: Number(document.getElementById("project-budget").value || 0),
    }),
  });
  event.target.reset();
  await refresh();
});

document.getElementById("org-membership-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  await api("/v1/admin/organization-memberships", {
    method: "POST",
    body: JSON.stringify({
      organization_id: document.getElementById("org-membership-org-id").value.trim(),
      user_id: document.getElementById("org-membership-user-id").value.trim(),
      role: document.getElementById("org-membership-role").value.trim(),
    }),
  });
  event.target.reset();
  await refresh();
});

document.getElementById("project-membership-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  await api("/v1/admin/project-memberships", {
    method: "POST",
    body: JSON.stringify({
      project_id: document.getElementById("project-membership-project-id").value.trim(),
      user_id: document.getElementById("project-membership-user-id").value.trim(),
      role: document.getElementById("project-membership-role").value.trim(),
    }),
  });
  event.target.reset();
  await refresh();
});

nodes.tokensTable.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-token-revoke]");
  if (!button) {
    return;
  }
  await api(`/v1/admin/tokens/${encodeURIComponent(button.dataset.tokenRevoke)}/revoke`, { method: "POST" });
  await refresh();
});

nodes.usersTable.addEventListener("click", async (event) => {
  const roleButton = event.target.closest("[data-user-role]");
  if (roleButton) {
    await api(`/v1/admin/users/${encodeURIComponent(roleButton.dataset.userRole)}/role`, {
      method: "POST",
      body: JSON.stringify({ role: roleButton.dataset.nextRole }),
    });
    await refresh();
    return;
  }
  const activeButton = event.target.closest("[data-user-active]");
  if (!activeButton) {
    return;
  }
  const action = activeButton.dataset.nextActive === "true" ? "activate" : "deactivate";
  await api(`/v1/admin/users/${encodeURIComponent(activeButton.dataset.userActive)}/${action}`, {
    method: "POST",
  });
  await refresh();
});

const IDENTITY_EXPLANATION = `✨ 我是 Makewand (v3.0) —— 零成本多模型 AI 订阅统一调度中枢。

我统合调度本机四大主流 AI 订阅服务：
• 🟢 Google AI Pro (Antigravity / AGY): 全局架构设计、复杂推理与闭环兜底
• 🔵 Claude Code (Anthropic): 高敏捷代码编写、多文件重构与实现
• 🔷 Codex CLI (OpenAI / gpt-6-astra): 独立红队代码审查与算法攻防
• 🟣 Muse Code (Meta / Llama): 辅助生成、沙箱验证与备用编码

核心能力与架构设计：
1. 智能意图路由：精准区分闲聊/问答（直接响应）与工程开发任务（多模型流水线），杜绝误触发程序检查或缺陷修复！
2. 跨模型联合调度：自动自适应规划、编码、红队盲审与 Auto-Fix 缺陷自愈。
3. 双模型沙箱竞速：在独立临时工作区中让两组模型同台竞技，由架构裁判评估。
4. 订阅配额健康监控：自动感知限流并无缝故障转移，零额外 API Token 成本。
5. 安全沙箱与预算搜索：内置 Bubblewrap 物理进程隔离与冷热文件防护搜索。`;

let playgroundHistory = [];
let userScrolledUpChat = false;

function classifyWebIntent(prompt) {
  const raw = (prompt || "").trim();
  const lower = raw.toLowerCase();

  // Action triggers take absolute precedence: only if user asks to create/edit/fix code
  const codingActionTriggers = [
    "写代码", "写一个", "写个", "写段", "帮我写", "编写", "实现", "创建文件",
    "生成代码", "重构", "修改代码", "改写代码", "落盘", "修bug", "修复",
    "解决bug", "补丁", "优化代码", "写单测", "编写测试", "写脚本", "生成脚本",
    "write code", "write a", "implement", "build a", "create a file", "fix bug",
    "patch", "refactor", "generate code", "write a test", "code a"
  ];
  if (codingActionTriggers.some((k) => lower.includes(k))) {
    return "task";
  }

  // Check for compound connectors (e.g. "顺便", "然后", "接着", "then", "and then")
  const compoundConnectors = [
    "顺便", "然后", "接着", "顺带", "并且", "同时", "再帮我", "帮我", "顺便帮我",
    "then ", "and then", "after that", "also "
  ];
  if (compoundConnectors.some((c) => lower.includes(c))) {
    return "explain";
  }

  const stripped = lower.replace(/[!！?？,，.。:：\s]/g, "");

  // Standalone greetings
  const pureGreetings = ["你好", "您好", "hi", "hello", "hey", "早上好", "下午好", "晚上好", "哈喽", "嗨", "打扰一下", "请问"];
  if (pureGreetings.includes(stripped)) {
    return "greeting";
  }
  for (const g of ["你好", "您好", "哈喽", "嗨", "hello", "hi"]) {
    if (lower.startsWith(g)) {
      const rem = lower.slice(g.length).replace(/^[ ,，!！?？;；\t\n]+/, "");
      if (rem.length > 0) {
        return "explain";
      }
    }
  }

  // Pure identity queries
  const identityQueries = [
    "你是谁", "你是什么", "你叫什么", "你叫啥", "你到底是", "你究竟是", "你何方神圣",
    "介绍一下自己", "介绍自己", "介绍一下你自己", "介绍下自己", "介绍下你自己",
    "自我介绍", "做个自我介绍", "做一下自我介绍",
    "你能做什么", "你能干什么", "你能干啥", "你有什么功能", "你有哪些功能", "你有什么用", "你主要用来做",
    "谁开发了你", "谁创造了你", "谁创建了你", "谁写了你", "你的作者是谁", "你的开发者是谁",
    "你是人类还是", "你是什么类型", "你属于哪种", "你是什么ai", "你是什么模型", "你是什么智能",
    "who are you", "what are you", "what is your name", "what's your name",
    "introduce yourself", "tell me about yourself", "what can you do", "what do you do",
    "who created you", "who made you", "who is your author"
  ];
  const taskVerbs = ["分析", "审查", "解释", "说明", "排查", "测试", "执行", "运行", "run", "test", "analyze", "check", "explain"];
  for (const q of identityQueries) {
    const compactQ = q.replace(/\s+/g, "");
    if (stripped.includes(compactQ)) {
      if (taskVerbs.some((tv) => lower.includes(tv))) {
        return "explain";
      }
      return "identity";
    }
  }

  if (lower.startsWith("/help") || lower === "help") return "help";
  if (lower.startsWith("/status") || lower.startsWith("/quota")) return "status";
  if (lower.startsWith("/models")) return "models";
  if (lower.startsWith("/review") || lower.includes("审查") || lower.includes("审计") || lower.includes("代码审计")) return "review";

  return "explain";
}

function appendChatMessage(role, text) {
  if (!nodes.chatHistory) return;
  const history = nodes.chatHistory;

  const bubble = document.createElement("div");
  bubble.className = `chat-bubble ${role}`;
  bubble.textContent = text;
  history.appendChild(bubble);

  if (!userScrolledUpChat) {
    history.scrollTop = history.scrollHeight;
  }
}

if (nodes.chatHistory) {
  nodes.chatHistory.addEventListener("scroll", () => {
    const history = nodes.chatHistory;
    const distanceFromBottom = history.scrollHeight - history.scrollTop - history.clientHeight;
    userScrolledUpChat = distanceFromBottom > 45;
  });
}

if (nodes.playgroundForm) {
  nodes.playgroundForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const input = nodes.playgroundInput.value.trim();
    if (!input) return;
    nodes.playgroundInput.value = "";

    userScrolledUpChat = false;
    appendChatMessage("user", input);

    const intent = classifyWebIntent(input);
    if (nodes.chatIntentBadge) {
      const intentLabels = {
        identity: "Identity / 身份说明",
        greeting: "Greeting / 问候",
        help: "Help / 帮助指南",
        status: "Status / 状态查询",
        models: "Models / 模型生态",
        task: "Task / 任务执行",
        review: "Review / 代码审查",
        explain: "Explain / 技术问答"
      };
      nodes.chatIntentBadge.textContent = intentLabels[intent] || "Active";
    }

    if (intent === "identity") {
      appendChatMessage("assistant", IDENTITY_EXPLANATION);
      return;
    }

    if (intent === "greeting") {
      appendChatMessage("assistant", "您好！我是 Makewand 联合调度助手。请问今天有什么可以帮您的？您可以随时向我咨询技术问题，或输入具体编码/审查需求。");
      return;
    }

    if (intent === "help") {
      appendChatMessage("assistant", "内置指令支持：\n• /status, /quota: 查看订阅健康度与配额\n• /models: 查看模型发现矩阵\n• 直接提问：例如‘你是谁’，‘解释Go语言channel’\n• 直接指派任务：例如‘实现LRU缓存并编写单测’");
      return;
    }

    if (intent === "status") {
      try {
        const dashboard = await api("/v1/admin/dashboard");
        appendChatMessage("status", `系统运行状态: Tokens: ${dashboard.tokens?.count || 0}, Users: ${dashboard.users?.count || 0}, Spend: $${Number(dashboard.usage?.summary?.total_cost_usd || 0).toFixed(2)}`);
      } catch (err) {
        appendChatMessage("status", `状态检查: ${err.message}`);
      }
      return;
    }

    if (intent === "models") {
      try {
        const modelsRes = await api("/v1/models");
        const list = (modelsRes.data || []).map((m) => m.id).join(", ");
        appendChatMessage("assistant", `可用模型/提供商矩阵: ${list || "balanced, codex, claude, agy, muse"}`);
      } catch (err) {
        appendChatMessage("assistant", "可用调度提供商: Google AI Pro (AGY), Claude Code, Codex CLI, Muse Code");
      }
      return;
    }

    // Maintain multi-turn dialog history context
    playgroundHistory.push({ role: "user", content: input });
    if (playgroundHistory.length > 20) {
      playgroundHistory = playgroundHistory.slice(-20);
    }

    // Session auth fallback: auto-issue a session token if logged in with session cookie
    if (!state.token && state.authMode === "session") {
      const stored = sessionStorage.getItem("mw_playground_token");
      if (stored) {
        state.token = stored;
      } else {
        try {
          const issued = await api("/v1/admin/tokens", {
            method: "POST",
            body: JSON.stringify({
              description: "Web Playground Session Token",
              scopes: ["chat:invoke", "models:read", "sessions:read", "sessions:write"]
            })
          });
          if (issued && issued.token) {
            state.token = issued.token;
            sessionStorage.setItem("mw_playground_token", issued.token);
          }
        } catch {
          // ignore
        }
      }
    }

    const intentDesc = intent === "task" ? "工程任务流水线" : (intent === "review" ? "代码审计" : "技术问答");
    appendChatMessage("status", `调度引擎处理中 (${intentDesc})...`);
    try {
      const response = await api("/v1/chat/completions", {
        method: "POST",
        body: JSON.stringify({
          mode: "balanced",
          messages: playgroundHistory
        })
      });
      const reply = response.choices?.[0]?.message?.content || "(模型未返回文本)";
      playgroundHistory.push({ role: "assistant", content: reply });
      const modelId = response.model ? `\n\n[来自 ${response.model}]` : "";
      appendChatMessage("assistant", reply + modelId);
    } catch (err) {
      appendChatMessage("status", `⚠️ 调用后端模型接口: ${err.message}\n(若未授权，请在登录页提供有效 Bearer Token 或管理员账号)`);
    }
  });

  if (nodes.playgroundInput) {
    let isComposing = false;
    nodes.playgroundInput.addEventListener("compositionstart", () => {
      isComposing = true;
    });
    nodes.playgroundInput.addEventListener("compositionend", () => {
      isComposing = false;
    });
    nodes.playgroundInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) {
        if (e.isComposing || isComposing || e.keyCode === 229) {
          return;
        }
        e.preventDefault();
        nodes.playgroundForm.dispatchEvent(new Event("submit"));
      }
    });
  }
}

if (nodes.chatClearBtn) {
  nodes.chatClearBtn.addEventListener("click", () => {
    playgroundHistory = [];
    userScrolledUpChat = false;
    if (nodes.chatHistory) {
      clearNode(nodes.chatHistory);
      appendChatMessage("system", "✨ 会话与日志记录已清空。您可以随时重新开始提问。");
    }
    if (nodes.chatIntentBadge) {
      nodes.chatIntentBadge.textContent = "Ready";
    }
  });
}

document.querySelectorAll(".nav-link").forEach((button) => {
  button.addEventListener("click", () => {
    document.querySelectorAll(".nav-link").forEach((item) => item.classList.remove("is-active"));
    document.querySelectorAll(".view-panel").forEach((panel) => panel.classList.remove("is-visible"));
    button.classList.add("is-active");
    document.querySelector(`[data-panel="${button.dataset.view}"]`).classList.add("is-visible");
  });
});

document.getElementById("sign-out").addEventListener("click", async () => {
  try {
    if (!state.token && state.authMode === "session") {
      await api("/v1/admin/session/logout", { method: "POST" });
    }
  } finally {
    clearSession();
    showApp(false);
  }
});

restoreSession().then((authenticated) => {
  if (!authenticated) {
    showApp(false);
    return;
  }
  showApp(true);
  return refresh();
}).catch((error) => {
  clearSession();
  showApp(false);
  nodes.loginError.textContent = error.message;
});
