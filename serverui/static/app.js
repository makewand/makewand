const state = {
  token: sessionStorage.getItem("makewand_admin_token") || "",
  authMode: sessionStorage.getItem("makewand_admin_auth_mode") || "",
  csrfToken: sessionStorage.getItem("makewand_admin_csrf") || "",
  userEmail: sessionStorage.getItem("makewand_admin_user_email") || "",
};

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

  if (state.token) {
    sessionStorage.setItem("makewand_admin_token", state.token);
  } else {
    sessionStorage.removeItem("makewand_admin_token");
  }
  if (state.authMode) {
    sessionStorage.setItem("makewand_admin_auth_mode", state.authMode);
  } else {
    sessionStorage.removeItem("makewand_admin_auth_mode");
  }
  if (state.csrfToken) {
    sessionStorage.setItem("makewand_admin_csrf", state.csrfToken);
  } else {
    sessionStorage.removeItem("makewand_admin_csrf");
  }
  if (state.userEmail) {
    sessionStorage.setItem("makewand_admin_user_email", state.userEmail);
  } else {
    sessionStorage.removeItem("makewand_admin_user_email");
  }

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

function renderTable(target, headers, rows) {
  if (!rows.length) {
    target.innerHTML = "<p class=\"hint-text\">No data yet.</p>";
    return;
  }
  const thead = `<thead><tr>${headers.map((header) => `<th>${header}</th>`).join("")}</tr></thead>`;
  const tbody = `<tbody>${rows.map((row) => `<tr>${row.map((cell) => `<td>${cell ?? ""}</td>`).join("")}</tr>`).join("")}</tbody>`;
  target.innerHTML = `<table>${thead}${tbody}</table>`;
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
      `<button class="ghost-button" data-user-role="${user.id}" data-next-role="${user.role === "admin" ? "member" : "admin"}">${user.role === "admin" ? "Make member" : "Promote admin"}</button>`,
      `<button class="ghost-button" data-user-active="${user.id}" data-next-active="${user.is_active ? "false" : "true"}">${user.is_active ? "Deactivate" : "Activate"}</button>`,
    ].join(" "),
  ]));

  renderTable(nodes.tokensTable, ["ID", "Description", "User", "Org", "Project", "Scopes", "Revoked", "Actions"], tokens.data.map((token) => [
    token.id,
    token.description || "",
    token.user_id || "",
    token.organization_id || "",
    token.project_id || "",
    (token.scopes || []).join(", "),
    token.revoked ? "yes" : "no",
    token.revoked ? "revoked" : `<button class="ghost-button" data-token-revoke="${token.id}">Revoke</button>`,
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
