#!/usr/bin/env bash
# ==============================================================================
# Makewand Official One-Line Installer
# Usage: curl -fsSL https://makewand.org/install.sh | bash
# ==============================================================================
set -euo pipefail

BOLD='\033[1m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${CYAN}${BOLD}"
cat << 'EOF'
  __  __       _                                 _ 
 |  \/  |     | |                               | |
 | \  / | __ _| | _____      ____ _ _ __   __| |
 | |\/| |/ _` | |/ / _ \ /\ / / _` | '_ \ / _` |
 | |  | | (_| |   <  __/\ V  / (_| | | | | (_| |
 |_|  |_|\__,_|_|\_\___| \_/ \__,_|_| |_|\__,_|
 Multi-Engine AI Orchestration Framework (v3.1.0)
EOF
echo -e "${NC}"

echo -e "🚀 正在安装 Makewand 多模型联合调度系统..."

# 1. Check requirements
if ! command -v git &>/dev/null; then
  echo -e "${RED}❌ 错误: 未检测到 git，请先安装 git 后重试。${NC}"
  exit 1
fi

if ! command -v python3 &>/dev/null; then
  echo -e "${RED}❌ 错误: 未检测到 python3，请先安装 python3 (>=3.9) 后重试。${NC}"
  exit 1
fi

PY_VER=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
echo -e "✓ Python 环境: Python ${PY_VER}"

# 2. Determine installation target
INSTALL_DIR="${HOME}/.makewand_app"
BIN_DIR="${HOME}/.local/bin"
mkdir -p "${BIN_DIR}"

if [ -d "${INSTALL_DIR}" ]; then
  echo -e "📦 发现已有安装目录，正在拉取最新代码更新..."
  git -C "${INSTALL_DIR}" pull origin master --ff-only 2>/dev/null || true
else
  echo -e "📦 正在检出 Makewand 官方仓库..."
  git clone --depth 1 https://github.com/makewand/makewand.git "${INSTALL_DIR}"
fi

# 3. Install Python dependencies & wrapper
echo -e "⚙️ 配置执行环境与命令软链接..."
cat << 'WRAPPER' > "${BIN_DIR}/makewand"
#!/usr/bin/env bash
INSTALL_DIR="${HOME}/.makewand_app"
export PYTHONPATH="${INSTALL_DIR}:${PYTHONPATH:-}"
exec python3 -m makewand.cli "$@"
WRAPPER
chmod +x "${BIN_DIR}/makewand"

# 4. Check PATH
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    echo -e "${YELLOW}⚠️ 注意: ${BIN_DIR} 尚未加入当前 PATH。${NC}"
    echo -e "建议在您的 ~/.bashrc 或 ~/.zshrc 中追加："
    echo -e "  export PATH=\"\$HOME/.local/bin:\$PATH\""
    export PATH="${BIN_DIR}:${PATH}"
    ;;
esac

# 5. Initialize config
mkdir -p "${HOME}/.config/makewand"
if [ ! -f "${HOME}/.config/makewand/config.json" ]; then
  cat << 'CFG' > "${HOME}/.config/makewand/config.json"
{
  "active_providers": ["claude", "codex", "agy", "grok", "muse"],
  "single_tool_mode": true,
  "local_model_enabled": false
}
CFG
  echo -e "✓ 已初始化用户配置文件: ~/.config/makewand/config.json"
fi

echo -e "\n${GREEN}${BOLD}🎉 Makewand v3.1.0 安装完成！${NC}"
echo -e "立即运行 ${CYAN}makewand${NC} 查看当前已激活的主流工具拓扑："
echo -e "  $ makewand\n"
"${BIN_DIR}/makewand" --version 2>/dev/null || true
