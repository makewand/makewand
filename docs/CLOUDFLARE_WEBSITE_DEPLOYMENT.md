# Makewand 官方网站 Cloudflare 托管与多域名部署手册

本文档指导如何将 Makewand 官方网站与文档中心部署至 Cloudflare，并绑定已注册的 `makewand.org` 与 `makewand.com` 双域名。

---

## 架构设计总览

| 模块 | 托管平台 | 目标域名 | 说明 |
|---|---|---|---|
| **官网与技术文档** | **Cloudflare Pages** | `makewand.org`<br>`www.makewand.org`<br>`makewand.com` | 极速边缘静态托管、自动免费 SSL、全球 CDN、零服务器成本 |
| **在线控制台 / API 网关（可选，不受支持的公网暴露）** | **Cloudflare Tunnel** | `console.makewand.org`<br>`api.makewand.org` | 把本机 `makewand serve`（端口 8080）暴露到公网；属于 [SERVER_ALPHA](SERVER_ALPHA.md) 明确不支持的部署形态，必须先满足第三部分的前置条件 |

---

## 第一部分：Cloudflare Pages 静态网站部署

网站源码位于仓库根目录的 `site/` 文件夹下，包含：
- `index.html`：现代极客暗黑风官网首页（多模型终端模拟器、特性展示、生态宇宙、能力对比矩阵）
- `docs.html`：完整技术文档中心
- `install.sh`：`curl -fsSL https://makewand.org/install.sh | bash` 一键安装脚本
- `assets/favicon.svg`：高清矢量星轨魔杖 Logo
- `_headers` 与 `_redirects`：Cloudflare 边缘安全响应头与路由重定向规则

### 方式 A：GitHub 自动集成部署（最推荐，一次配置永久免维护）

1. 登录 [Cloudflare 控制台](https://dash.cloudflare.com/)。
2. 左侧导航栏进入 **Compute (Workers) > Workers & Pages**，点击 **Create** > **Pages** > **Connect to Git**。
3. 选择 GitHub 仓库 `makewand/makewand`：
   - **Project Name**：`makewand`
   - **Production branch**：`master`
   - **Framework preset**：`None`（纯静态）
   - **Build command**：（留空）
   - **Build output directory**：`site`
4. 点击 **Save and Deploy**。
   - 几秒内即可生成官方 Pages 域名：`https://makewand.pages.dev`。
   - 后续任何推送至 GitHub `master` 分支的提交，Cloudflare 均会自动秒级构建更新。

---

### 方式 B：通过 Wrangler CLI 本地一键发布

仓库已提供自动化部署脚本 `scripts/deploy_website.sh`：

```bash
# 1. 登录 Cloudflare 授权（仅需一次）
npx wrangler login

# 2. 一键发布 site/ 目录至 Cloudflare Pages
bash scripts/deploy_website.sh

# （可选）在本地预览效果
bash scripts/deploy_website.sh --preview 8090
```

---

## 第二部分：双域名绑定与 DNS 配置 (`makewand.org` / `makewand.com`)

因为您的两个域名都在 Cloudflare 管理，绑定过程全部在控制台内自动化完成：

### 1. 绑定主域名 `makewand.org`
1. 在 Pages 项目详情页中，切换至 **Custom domains**（自定义域）标签。
2. 点击 **Set up a custom domain**。
3. 输入 `makewand.org`，点击继续。Cloudflare 会自动添加一条 CNAME DNS 记录指向 Pages 节点。
4. 再次点击 **Set up a custom domain**，输入 `www.makewand.org`。

### 2. 绑定或重定向 `makewand.com`
您可以通过以下两种方案之一配置 `makewand.com`：

- **方案一：作为完全对等镜像域名直接绑定**
  - 在同一 Pages 项目的 **Custom domains** 中，再次添加 `makewand.com` 与 `www.makewand.com`。
  - 用户无论访问 `.org` 还是 `.com` 均能直接浏览完整站点。

- **方案二：301 规范化重定向至 `.org`（推荐 SEO 规范做法）**
  - 进入 Cloudflare 域名控制台，选择 `makewand.com` 域名。
  - 进入 **Rules (规则) > Redirect Rules (重定向规则)**。
  - 创建规则：当用户请求 `makewand.com/*` 时，301 永久重定向至 `https://makewand.org/$1`。

---

## 第三部分：Cloudflare Tunnel 映射后台管理与 API（可选，不受支持）

若您需要在公网访问本机的 `makewand serve` Web 控制台（端口 8080）与 OpenAI 兼容 API，请先读完本节风险与前置条件。

### 风险说明

- **公网部署不在支持范围内。** [SERVER_ALPHA](SERVER_ALPHA.md) 把服务端定位为个人或受信网络使用的 alpha 组件，"Public internet deployment" 明确不受支持；隧道会把 `/admin` 控制台、`/v1/admin/*` 管理 API、登录接口与模型调用接口一起暴露给整个互联网。
- Tunnel 只解决"无需公网 IP"与边缘 TLS，**不提供身份认证**。不加访问控制时，任何人都能访问登录页并对管理员账号做在线口令猜测，只受登录限速与 Argon2id 成本约束。
- 模型调用按服务端配置的订阅或 API key 计费，泄露的 token 会直接消耗您的额度。
- 源站是 `127.0.0.1:8080` 上的明文 HTTP，TLS 只在 Cloudflare 边缘终止，本机其他进程也能访问该端口。

### 前置条件（全部满足后再启用）

1. **在 Cloudflare Zero Trust 为 `console.makewand.org` 配置 Cloudflare Access 应用**，只允许指定身份（邮箱/IdP 组）访问；`/admin` 与 `/v1/admin/` 不得在无 Access 保护的主机名上可达。`api.makewand.org` 若只给程序调用，使用 Access Service Token 或至少保证只发放带配额的 scoped token。
2. **以 `--trusted-proxy 127.0.0.1` 启动服务。** `cloudflared` 从本机回环地址连到源站，并把真实客户端 IP 追加到 `X-Forwarded-For` 末尾；服务端从右向左跳过可信代理取第一个不可信地址，登录与注册限速才会按真实客户端生效。不配置时所有请求共享 `127.0.0.1` 这一个限速键，一个攻击者即可把所有人锁在登录之外。
3. **只监听回环地址**：`makewand serve --listen 127.0.0.1:8080 ...`，不要使用 `--unsafe-no-tls` 绑定 `0.0.0.0`。
4. **不要开启 `--enable-registration`**；账号由管理员创建，管理员使用强口令。
5. 客户端 token 按最小权限发放并设置配额（`makewand token issue --max-requests-per-day ... --max-cost-usd-per-month ...`），启用审计日志（`--audit-log` 或 `MAKEWAND_SERVER_AUDIT_LOG=1`），并定期检查 `/v1/admin/audit/events`。
6. 若 API 通过付费 API key 服务（`MAKEWAND_API_POLICY=allow_paid`），为组织/项目设置月度预算。

启动示例：

```bash
makewand serve --listen 127.0.0.1:8080 --enable-users --trusted-proxy 127.0.0.1 --audit-log ~/.config/makewand/server/audit.jsonl
```

### 1. 创建 Tunnel
```bash
cloudflared tunnel create makewand-gateway
```
记录生成的 `<Tunnel-UUID>`，对应凭据文件存放在 `~/.cloudflared/<Tunnel-UUID>.json`。

### 2. 配置 Ingress
参考仓库中的 `deploy/cloudflare-tunnel.makewand.yml`：
```yaml
tunnel: <Tunnel-UUID>
credentials-file: ~/.cloudflared/<Tunnel-UUID>.json
protocol: http2

ingress:
  - hostname: console.makewand.org
    service: http://127.0.0.1:8080
  - hostname: api.makewand.org
    service: http://127.0.0.1:8080
  - service: http_status:404
```

### 3. 配置 DNS 路由并启动
```bash
# 绑定路由
cloudflared tunnel route dns makewand-gateway console.makewand.org
cloudflared tunnel route dns makewand-gateway api.makewand.org

# 启动隧道
cloudflared tunnel --config deploy/cloudflare-tunnel.makewand.yml run
```
配置成功后，先通过 Cloudflare Access 登录，再访问 `https://console.makewand.org/admin` 进入 Makewand Web 控制台。未配置 Access 前不要执行 `cloudflared tunnel ... run`。

---

## 常用运维排查

- **测试 HTTP 响应头与 HTTPS**：
  ```bash
  curl -I https://makewand.org
  curl -I https://makewand.com
  ```
- **测试一键安装脚本直达性**：
  ```bash
  curl -fsSL https://makewand.org/install.sh | head -n 20
  ```
