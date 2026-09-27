#!/usr/bin/env bash
# ai-dispatch 回归测试:只用桩 CLI,不调用真实模型
set -u
T="$(cd "$(dirname "$0")" && pwd)"; AD="$T/ai-dispatch"
W="${TMPDIR:-/tmp}/aid-test-$$"; rm -rf "$W"; mkdir -p "$W/stubs" "$W/home"
trap 'rm -rf "$W"' EXIT INT TERM
export AI_DISPATCH_HOME="$W/runs"
PASS=0; FAIL=0
ok()  { echo "  ✔ $1"; PASS=$((PASS+1)); }
bad() { echo "  ✘ $1"; FAIL=$((FAIL+1)); }
expect_rc() { local want=$1 desc=$2; shift 2; "$@" >/dev/null 2>&1 </dev/null; local rc=$?; [[ $rc == "$want" ]] && ok "$desc (rc=$rc)" || bad "$desc (期望 $want 实得 $rc)"; }
jget() { python3 -c "import json,sys;d=json.loads(sys.stdin.read().strip().splitlines()[-1]);print(d.get('$1'))"; }

# ---- 桩 CLI ----
cat > "$W/stubs/claude" <<'EOF'
#!/usr/bin/env bash
cat > /dev/null   # 吃掉 stdin 里的提示词
case "${STUB_MODE:-ok}" in
  ok)     echo '{"type":"result","subtype":"success","is_error":false,"result":"STUB-OK","num_turns":1}' ;;
  sleep)  sleep "${STUB_SLEEP:-40}"; echo '{"result":"late"}' ;;
  write)  echo "def sub(a,b): return a-b" >> calc.py; echo '{"is_error":false,"result":"done"}' ;;
  commit) echo new > added.txt; git add added.txt; git -c user.name=s -c user.email=s@s commit -qm stub; echo '{"is_error":false,"result":"committed"}' ;;
  oom)    python3 -c 'b=bytearray(400*1024*1024); print(len(b))'; echo '{"result":"x"}' ;;
  quotaerr) echo '{"is_error":true,"result":"Claude AI usage limit reached|1790000000"}'; exit 1 ;;
  bodyquota) echo '{"is_error":false,"result":"讨论了 rate limit 和 429 的处理"}' ;;
  surrogate) printf '{"is_error":false,"result":"bad \\ud83d end"}\n' ;;
  sofalse) echo '{"is_error":false,"result":"The answer is false.","structured_output":false}' ;;
esac
EOF
cat > "$W/stubs/muse" <<'EOF'
#!/usr/bin/env bash
# 找出 --workspace 和 --prompt-file
while (($#)); do case "$1" in --workspace) WS="$2"; shift;; --prompt-file) PF="$2"; shift;; esac; shift; done
echo "muse: workspace root: ${WS:-?} (explicit)" >&2
case "${STUB_MODE:-ok}" in
  ok)   printf '{"payload_type":"run.terminal.completed","payload":{"text":"MUSE-OK:%s","terminal":"completed"}}\n' "$(head -c 20 "$PF")" ;;
  fail1) echo "segfault in tool runner" >&2; exit 1 ;;
  termfailed) echo '{"payload_type":"run.terminal.failed","payload":{"reason":"rate_limited: 429","terminal":"failed"}}' ;;
esac
EOF
cat > "$W/stubs/grok" <<'EOF'
#!/usr/bin/env bash
case "$1" in --single=*) P="${1#--single=}";; *) echo "error: unexpected argument '$1'" >&2; exit 2;; esac
printf '{"type":"result","subtype":"success","result":"GROK:%s"}\n' "${P:0:12}"
EOF
# Unit mode does not require a logged-in systemd user session. These stubs
# validate scope arguments and result classification; real cgroup/OOM coverage
# is available explicitly with AI_DISPATCH_TEST_LIVE_SYSTEMD=1.
if [[ "${AI_DISPATCH_TEST_LIVE_SYSTEMD:-0}" != 1 ]]; then
  cat > "$W/stubs/systemd-run" <<'STUB'
#!/usr/bin/env bash
set -eu
memory=0; swap=0
while (($#)); do
  case "$1" in
    MemoryMax=*) memory=1 ;;
    MemorySwapMax=0) swap=1 ;;
    --) shift; break ;;
  esac
  shift
done
[[ "$1" == true ]] && exit 0
[[ "$memory" == 1 && "$swap" == 1 ]] || exit 7
[[ "${STUB_MODE:-}" == oom ]] && exit 137
exec "$@"
STUB
  cat > "$W/stubs/systemctl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"show "* ]]; then
  if [[ "${STUB_MODE:-}" == oom ]]; then echo oom-kill; else echo success; fi
fi
exit 0
STUB
  echo 'Dispatch unit tests: stub systemd scope; set AI_DISPATCH_TEST_LIVE_SYSTEMD=1 for live cgroup tests.'
fi
chmod +x "$W/stubs/"*
export PATH="$W/stubs:$PATH"

mkrepo() { rm -rf "$1"; mkdir -p "$1"; git -C "$1" init -q; printf 'def add(a,b):\n    return a+b\n' > "$1/calc.py"; git -C "$1" add .; git -C "$1" -c user.name=t -c user.email=t@t commit -qm init; }
R="$W/repo"; mkrepo "$R"

echo "== 参数边界"
expect_rc 2 "-C 缺值"            "$AD" claude -C
expect_rc 2 "-t 缺值"            "$AD" claude hello -t
expect_rc 2 "-t 0"               "$AD" claude -t 0 hello
expect_rc 2 "空提示词"           "$AD" claude ""
expect_rc 2 "纯空白提示词"       "$AD" claude "   "
expect_rc 2 "-f 不存在"          "$AD" claude -f /nonexistent.txt
expect_rc 2 "-f 是目录"          "$AD" claude -f /tmp
expect_rc 2 "未知选项"           "$AD" claude --bogus x
expect_rc 6 "grok --rw 拒绝"     "$AD" grok --rw -C "$R" x
expect_rc 0 "-h 在中间"          "$AD" claude -C "$R" -h
expect_rc 6 "--rw 非 git 目录"   "$AD" claude --rw -C "$W/home" x
mkdir -p "$W/empty"; git -C "$W/empty" init -q
expect_rc 2 "--rw 空仓库"        "$AD" claude --rw -C "$W/empty" x
mkdir -p "$R/newpkg"
expect_rc 2 "--rw 子目录不在 HEAD" "$AD" claude --rw -C "$R/newpkg" x
rmdir "$R/newpkg"
n=$(ls "$W/runs" 2>/dev/null | wc -l); [[ $n == 0 ]] && ok "上面的失败没有留下运行目录" || bad "留下了 $n 个运行目录"

echo "== dry-run"
for e in claude codex muse grok; do
  out=$("$AD" $e -C "$R" --dry-run -- "- 以横杠开头的提示词" 2>&1); rc=$?
  [[ $rc == 0 ]] && ok "$e dry-run" || bad "$e dry-run rc=$rc: $out"
done
n=$(ls "$W/runs" 2>/dev/null | wc -l); [[ $n == 0 ]] && ok "dry-run 不留运行目录" || bad "dry-run 留下 $n 个目录"
"$AD" grok -C "$R" --dry-run -- "- 列表" | grep -q -- '--single=\\<prompt:' && ok "grok 提示词用 --single= 且日志已掩码" || bad "grok 提示词掩码"
"$AD" claude --rw -C "$R" --dry-run x | grep -q 'sandbox' && ok "claude 带沙箱设置" || bad "claude 缺沙箱设置"
"$AD" muse -C "$R" --dry-run x | grep -q -- '--prompt-file' && ok "muse 用 --prompt-file" || bad "muse 未用 --prompt-file"

echo "== 结果与状态"
o=$(STUB_MODE=ok "$AD" claude -C "$R" hi); [[ $(jget status <<<"$o") == ok ]] && ok "claude ok" || bad "claude ok: $o"
o=$(STUB_MODE=quotaerr "$AD" claude -C "$R" hi); [[ $(jget status <<<"$o") == quota ]] && ok "claude 额度报错→quota" || bad "quota: $o"
o=$(STUB_MODE=bodyquota "$AD" claude -C "$R" hi); [[ $(jget status <<<"$o") == ok ]] && ok "正文提到 rate limit/429 不误判" || bad "bodyquota: $o"
o=$(STUB_MODE=surrogate "$AD" claude -C "$R" hi); rc=$?; [[ $rc == 0 && -n "$o" ]] && ok "截断的代理对不崩溃" || bad "surrogate rc=$rc: $o"
sch="$W/bool.json"; echo '{"type":"boolean"}' > "$sch"
o=$(STUB_MODE=sofalse "$AD" claude -C "$R" --schema "$sch" hi); r=$(cat "$(jget result <<<"$o")"); [[ "$r" == false ]] && ok "structured_output=false 保留" || bad "sofalse: $r"
mkrepo "$W/quota-service"
o=$(STUB_MODE=fail1 "$AD" muse -C "$W/quota-service" hi); [[ $(jget status <<<"$o") == engine_error ]] && ok "muse 路径含 quota 不误判" || bad "muse path quota: $o"
o=$(STUB_MODE=termfailed "$AD" muse -C "$R" hi); [[ $(jget status <<<"$o") == quota ]] && ok "muse run.terminal.failed(429)→quota" || bad "termfailed: $o"
o=$(STUB_MODE=ok "$AD" muse -C "$R" -- "- 横杠开头"); r=$(cat "$(jget result <<<"$o")"); [[ "$r" == "MUSE-OK:- 横杠开头"* ]] && ok "muse 提示词经文件原样传入" || bad "muse prompt: $r"
o=$("$AD" grok -C "$R" -- "- 列表项"); r=$(cat "$(jget result <<<"$o")"); [[ "$r" == "GROK:- 列表项"* ]] && ok "grok 横杠开头提示词" || bad "grok dash: $o"
printf 'DIFF-BODY\n' > "$W/pipe.txt"
o=$(STUB_MODE=ok "$AD" claude -C "$R" "review" < "$W/pipe.txt"); grep -q DIFF-BODY "$(jget run_dir <<<"$o")/prompt.txt" && ok "参数+管道都进提示词" || bad "pipe+arg"
o=$(STUB_MODE=oom AI_DISPATCH_MEM=64M "$AD" claude -C "$R" hi); [[ $(jget status <<<"$o") == killed ]] && ok "OOM→killed(不是 timeout)" || bad "oom: $o"
o=$(STUB_MODE=sleep STUB_SLEEP=10 "$AD" claude -C "$R" -t 2 hi); [[ $(jget status <<<"$o") == timeout ]] && ok "超时→timeout" || bad "timeout: $o"

echo "== 可写模式"
h0=$(sha256sum "$R/calc.py")
o=$(STUB_MODE=write "$AD" claude --rw -C "$R" x); p=$(jget patch <<<"$o")
[[ -s "$p" ]] && ok "rw 产出补丁" || bad "rw 补丁: $o"
[[ "$(sha256sum "$R/calc.py")" == "$h0" ]] && ok "原仓库未改动" || bad "原仓库被改"
o=$(STUB_MODE=commit "$AD" claude --rw -C "$R" x); p=$(jget patch <<<"$o")
[[ -s "$p" ]] && grep -q added.txt "$p" && ok "引擎自己 commit 后补丁不丢" || bad "commit 补丁: $o"
[[ $(git -C "$R" worktree list | wc -l) == 1 ]] && ok "worktree 已清理" || bad "worktree 残留"
[[ -z "$(git -C "$R" status --porcelain)" ]] && ok "原仓库 status 干净" || bad "原仓库不干净"
cd "$W/home" && o=$(AI_DISPATCH_HOME=rel STUB_MODE=write "$AD" claude --rw -C "$R" x); cd - >/dev/null
[[ "$(jget run_dir <<<"$o")" == "$W/home/rel/"* && -z "$(git -C "$R" status --porcelain)" ]] && ok "相对 AI_DISPATCH_HOME 转成绝对路径" || bad "相对 HOME: $o"

echo "== 信号转发"
setsid bash -c "STUB_MODE=sleep STUB_SLEEP=67 '$AD' claude --rw -C '$R' -t 50 x > '$W/sig.out' 2>&1" &
sleep 3; pg=$(pgrep -f "ai-dispatch claude --rw -C $R -t 50" | head -1)
[[ -n "$pg" ]] && kill -TERM -- -"$(ps -o pgid= -p "$pg" | tr -d ' ')"
sleep 4
pgrep -fx "sleep 67" >/dev/null && bad "引擎在 ai-dispatch 被杀后仍存活" || ok "引擎随 ai-dispatch 一起退出"
grep -q '"status":"cancelled"' "$W/sig.out" && ok "被杀时输出 cancelled 的 JSON" || bad "没有 cancelled JSON: $(cat "$W/sig.out")"
[[ $(git -C "$R" worktree list | wc -l) == 1 ]] && ok "被杀后 worktree 已清理" || bad "被杀后 worktree 残留"

echo; echo "通过 $PASS,失败 $FAIL"
((FAIL == 0))
