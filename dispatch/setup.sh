#!/usr/bin/env bash
# 2026-09-26 调度层改造:三步可分开执行,每步都先备份(后缀 .bak-20260926)
#   bash setup.sh muse           修复 muse 出网管控(需 sudo):开机加载失败 + 封装改为 fail-closed
#   bash setup.sh dispatch       安装 ~/.local/bin/ai-dispatch(统一派发脚本)
#   bash setup.sh codex-rules    让 3 个 codex 账号在仓库没有 AGENTS.md 时读 CLAUDE.md
#   bash setup.sh codex-global   另外把 ~/.claude/CLAUDE.md 作为 codex 全局规则(内容会随每次请求发给 OpenAI)
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
TS=bak-20260926

case "${1:-}" in
  muse)
    bash "$HERE/muse-guard/apply.sh" ;;

  dispatch)
    DST="$HOME/.local/bin/ai-dispatch"
    SRC="$HERE/ai-dispatch"
    [[ -f "$SRC" ]] || SRC="$HERE/dispatch/ai-dispatch"
    [[ -e "$DST" && ! -e "$DST.$TS" ]] && cp -a "$DST" "$DST.$TS"
    install -m 0755 "$SRC" "$DST"
    echo "已安装 $DST"; ai-dispatch --help | head -5 ;;

  codex-rules)
    LINE='project_doc_fallback_filenames = ["CLAUDE.md"]  # 仓库没有 AGENTS.md 时读 CLAUDE.md(2026-09-26,与 Claude 共用规则)'
    for h in "$HOME/.codex" "$HOME/.codex-2" "$HOME/.codex-3"; do
      f="$h/config.toml"; [[ -f "$f" ]] || { echo "跳过 $h(没有 config.toml)"; continue; }
      if grep -q '^project_doc_fallback_filenames' "$f"; then echo "$h:已配置,跳过"; continue; fi
      cp -a "$f" "$f.$TS"
      # 必须写在第一个 [table] 之前才是顶层键,这里直接插到文件第一行
      { echo "$LINE"; cat "$f.$TS"; } > "$f"
      echo "$h:已加入 project_doc_fallback_filenames"
    done
    echo "验证(不调用模型):"
    ( cd /path/to/workspace/sample_project_1 2>/dev/null && codex debug prompt-input 2>/dev/null | grep -c 'AGENTS.md instructions' ) \
      || echo "  (codex debug prompt-input 不可用,跳过验证)" ;;

  codex-global)
    echo "注意:~/.claude/CLAUDE.md 含生产 IP、内网 Gitea 地址和团队规定,链接后会随每次 codex 请求发给 OpenAI。"
    read -r -p "确认继续? [y/N] " ans; [[ "$ans" == y || "$ans" == Y ]] || { echo "已取消"; exit 0; }
    for h in "$HOME/.codex" "$HOME/.codex-2" "$HOME/.codex-3"; do
      [[ -d "$h" ]] || continue
      if [[ -e "$h/AGENTS.md" ]]; then echo "$h/AGENTS.md 已存在,跳过"; continue; fi
      ln -s "$HOME/.claude/CLAUDE.md" "$h/AGENTS.md"; echo "$h/AGENTS.md -> ~/.claude/CLAUDE.md"
    done ;;

  *) sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
