#!/usr/bin/env bash
# 修复 muse-guard 开机加载失败 + 让 muse 封装在管控未生效时拒绝启动。
# 用法: bash apply.sh        (需要 sudo;会先备份原文件,后缀 .bak-20260926)
# 回滚: bash apply.sh --rollback
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
TS=bak-20260926
NFT=/etc/nftables-muse-guard.nft
UNIT=/etc/systemd/system/muse-guard.service
WRAP="$HOME/.local/bin/muse"

if [[ "${1:-}" == "--rollback" ]]; then
  sudo cp -a "$NFT.$TS" "$NFT"; sudo cp -a "$UNIT.$TS" "$UNIT"; cp -a "$WRAP.$TS" "$WRAP"
  sudo systemctl daemon-reload; sudo systemctl disable muse-guard; sudo systemctl enable muse-guard
  sudo systemctl restart muse-guard || true
  echo "已回滚"; exit 0
fi

echo "== 1/5 备份"
[[ -e "$NFT.$TS"  ]] || sudo cp -a "$NFT"  "$NFT.$TS"
[[ -e "$UNIT.$TS" ]] || sudo cp -a "$UNIT" "$UNIT.$TS"
[[ -e "$WRAP.$TS" ]] || cp -a "$WRAP" "$WRAP.$TS"

echo "== 2/5 语法检查新规则"
sudo nft -c -f "$HERE/nftables-muse-guard.nft"

echo "== 3/5 安装规则、服务与封装"
sudo install -m 0644 "$HERE/nftables-muse-guard.nft" "$NFT"
sudo install -m 0644 "$HERE/muse-guard.service" "$UNIT"
install -m 0755 "$HERE/muse" "$WRAP"
sudo systemctl daemon-reload
# 安装目标从 multi-user.target 换成 user@1000.service,要先 disable 再 enable 重建链接
sudo systemctl disable muse-guard
sudo systemctl enable muse-guard
sudo systemctl restart muse-guard
systemctl is-active muse-guard

echo "== 4/5 实测管控(不调用 muse,只用 bash /dev/tcp)"
probe() { timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null && echo 通 || echo 不通; }
inslice() { systemd-run --user --scope --quiet --slice=muse --collect -- timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null && echo 通 || echo 不通; }
printf '  slice 外 7891: %s（期望 通）\n' "$(probe 7891)"
printf '  slice 内 7891: %s（期望 不通）\n' "$(inslice 7891)"
printf '  slice 内 7890: %s（期望 通）\n' "$(inslice 7890)"

echo "== 5/5 模拟「开机时 slice 还不存在」与「slice 重建」"
if systemd-cgls --user-unit muse.slice 2>/dev/null | grep -q scope; then
  echo "  muse.slice 里有进程在跑,跳过这一步(会杀掉它们)"
else
  systemctl --user stop muse.slice
  sudo systemctl restart muse-guard       # 服务应自己先建 slice 再加载
  printf '  服务重启后 slice 内 7891: %s（期望 不通）\n' "$(inslice 7891)"
  systemctl --user stop muse.slice        # 重建 slice,旧规则失效
  printf '  slice 重建后(未重载) slice 内 7891: %s（这里显示 通 就说明旧规则已失效）\n' "$(inslice 7891)"
  "$WRAP" --version >/dev/null 2>&1 && echo "  muse 封装启动成功(已自愈)" || echo "  muse 封装拒绝启动或启动失败,请看上面输出"
  printf '  自愈后 slice 内 7891: %s（期望 不通）\n' "$(inslice 7891)"
fi
echo "完成。回滚: bash $HERE/apply.sh --rollback"
