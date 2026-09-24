# Makewand 官方网站 Cloudflare 托管与多域名部署手册

本文档指导如何将 Makewand 官方网站与文档中心部署至 Cloudflare，并绑定已注册的 `makewand.org` 与 `makewand.com` 双域名。

---

## 架构设计总览

| 模块 | 托管平台 | 目标域名 | 说明 |
|---|---|---|---|
| **官网与技术文档** | **Cloudflare Pages** | `makewand.org`<br>`www.makewand.org`<br>`makewand.com` | 极速边缘静态托管、自动免费 SSL、全球 CDN、零服务器成本 |
| **在线控制台 / API 网关** | **Cloudflare Tunnel** | `console.makewand.org`<br>`api.makewand.org` | 安全穿透内网 `makewand serve` (端口 8080)，免公网 IP，内置安全防御 |

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

## 第三部分：Cloudflare Tunnel 映射后台管理与 API（可选）

若您需要在公网访问本机的 `makewand serve` Web 控制台（端口 8080）与 OpenAI 兼容 API：

### 1. 创建 Tunnel
```bash
cloudflared tunnel create makewand-gateway
```
记录生成的 `<Tunnel-UUID>`，对应凭据文件存放在 `~/.cloudflared/<Tunnel-UUID>.json`。

### 2. 配置 Ingress
参考仓库中的 `deploy/cloudflare-tunnel.makewand.yml`：
```yaml
tunnel: <Tunnel-UUID>
credentials-file: /home/user/.cloudflared/<Tunnel-UUID>.json
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
配置成功后，访问 `https://console.makewand.org/admin` 即可直接进入 Makewand Web 控制台。

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
