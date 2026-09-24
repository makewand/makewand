#!/usr/bin/env bash
# ==============================================================================
# Deploy Makewand Website to Cloudflare Pages
# Supports: makewand.org & makewand.com
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SITE_DIR="${REPO_ROOT}/site"

echo "========================================================"
echo " 🌐 Makewand Cloudflare Pages Deployment Pipeline"
echo " Target Domains: makewand.org & makewand.com"
echo "========================================================"

if [ ! -d "${SITE_DIR}" ]; then
  echo "❌ 错误: 网站目录不存在: ${SITE_DIR}"
  exit 1
fi

# Optional local preview
if [[ "${1:-}" == "--preview" || "${1:-}" == "preview" ]]; then
  PORT="${2:-8090}"
  echo "🚀 启动本地预览服务: http://127.0.0.1:${PORT}"
  cd "${SITE_DIR}"
  python3 -m http.server "${PORT}"
  exit 0
fi

# Pre-flight check
echo "🔍 检查网站静态资源完整性..."
for f in index.html docs.html styles.css main.js install.sh assets/favicon.svg _headers _redirects; do
  if [ ! -f "${SITE_DIR}/${f}" ]; then
    echo "❌ 缺少关键文件: site/${f}"
    exit 1
  fi
done
echo "✓ 静态资源检查全部通过。"

# Account ID fallback (matches local cloudflare account if present)
export CLOUDFLARE_ACCOUNT_ID="${CLOUDFLARE_ACCOUNT_ID:-d6350176a532ed1e84a0699962adf585}"

echo "📦 正在向 Cloudflare Pages 发布部署..."
if command -v wrangler &>/dev/null; then
  WRANGLER_BIN="wrangler"
else
  WRANGLER_BIN="npx wrangler"
fi

if ${WRANGLER_BIN} pages deploy "${SITE_DIR}" \
    --project-name=makewand \
    --branch=master \
    --commit-dirty=true; then
  echo "========================================================"
  echo " 🎉 Cloudflare Pages 部署成功！"
  echo " 默认 Pages 域名: https://makewand.pages.dev"
  echo " 自定义域名绑定:  https://makewand.org"
  echo " 商业域名别名:    https://makewand.com"
  echo "========================================================"
else
  echo ""
  echo "⚠️ 注意: 若提示认证失败，请先执行:"
  echo "  export CLOUDFLARE_API_TOKEN=\"<your-cloudflare-api-token>\""
  echo "或者在本地终端运行: wrangler login 完成 Cloudflare 授权。"
  echo "本地静态资源已完全就绪，随时可通过 'bash scripts/deploy_website.sh --preview' 在本地预览。"
  exit 1
fi
