#!/bin/bash
# benchmarks/run.sh — 自动化基准测试执行器
#
# 用法:
#   ./benchmarks/run.sh                    # 运行全部
#   ./benchmarks/run.sh countdown          # 只跑单个案例
#   ./benchmarks/run.sh todo balanced      # 只跑指定案例+模式
#
# 依赖: makewand, gemini, codex, claude 已在 PATH 中
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RESULTS_DIR="$PROJECT_ROOT/benchmarks/results/$(date +%Y%m%d-%H%M%S)"
PROMPTS_DIR="$SCRIPT_DIR/prompts"

FILTER_CASE="${1:-}"
FILTER_MODE="${2:-}"

ALL_CASES=("countdown" "todo" "blog-gen" "chatroom" "expense-tracker")
ALL_MODES=("fast" "balanced" "power")
DIRECT_TOOLS=("gemini" "codex" "claude")

TIMEOUT=600  # 10 分钟超时

mkdir -p "$RESULTS_DIR"
echo "结果目录: $RESULTS_DIR"
echo ""

# --- 工具检测 ---
check_tool() {
    if command -v "$1" &>/dev/null; then
        echo "  [x] $1"
        return 0
    else
        echo "  [ ] $1 (未找到, 跳过)"
        return 1
    fi
}

echo "检测工具:"
check_tool makewand || true
HAVE_GEMINI=false; check_tool gemini && HAVE_GEMINI=true || true
HAVE_CODEX=false;  check_tool codex  && HAVE_CODEX=true  || true
HAVE_CLAUDE=false; check_tool claude && HAVE_CLAUDE=true || true
echo ""

should_run_case() {
    [ -z "$FILTER_CASE" ] || [ "$FILTER_CASE" = "$1" ]
}

should_run_mode() {
    [ -z "$FILTER_MODE" ] || [ "$FILTER_MODE" = "$1" ]
}

# --- makewand 模式测试 ---
# 注意: `makewand new` 是交互式 TUI 程序, 无法完全自动化。
# 以下命令用 --debug 启动, 需要手动操作:
#   1. 选择 "Custom" (最后一个模板)
#   2. 粘贴 prompt 文本
#   3. 确认 plan → confirm → 等待 build 完成
#   4. Y 安装依赖 → Y 运行测试 → Ctrl+C 退出
#
# 如果已有 `makewand chat` 的非交互模式, 可替换为:
#   echo "$prompt" | makewand chat "$dir" --mode "$mode"

for case_name in "${ALL_CASES[@]}"; do
    should_run_case "$case_name" || continue
    prompt_file="$PROMPTS_DIR/${case_name}.txt"
    [ -f "$prompt_file" ] || { echo "SKIP: 缺少 $prompt_file"; continue; }

    for mode in "${ALL_MODES[@]}"; do
        should_run_mode "$mode" || continue
        dir="$RESULTS_DIR/${case_name}/makewand-${mode}"
        mkdir -p "$dir"

        echo "=== $case_name / makewand --mode $mode ==="
        echo "  目录: $dir"
        echo "  请手动运行:"
        echo "    cd $dir && makewand new --mode $mode --debug"
        echo "    # 选 Custom → 粘贴 $prompt_file 的内容"
        echo ""
    done
done

# --- 直接工具对比 ---
for case_name in "${ALL_CASES[@]}"; do
    should_run_case "$case_name" || continue
    prompt_file="$PROMPTS_DIR/${case_name}.txt"
    [ -f "$prompt_file" ] || continue
    prompt=$(cat "$prompt_file")

    # Gemini CLI
    if $HAVE_GEMINI && should_run_mode "gemini"; then
        dir="$RESULTS_DIR/${case_name}/gemini-direct"
        mkdir -p "$dir"
        echo "=== $case_name / gemini direct ==="
        (
            cd "$dir"
            timeout "$TIMEOUT" gemini -p "$prompt" 2>"trace.log" || echo "TIMEOUT/ERROR"
        )
        echo "  Done: $dir"
    fi

    # Codex CLI
    if $HAVE_CODEX && should_run_mode "codex"; then
        dir="$RESULTS_DIR/${case_name}/codex-direct"
        mkdir -p "$dir"
        echo "=== $case_name / codex direct ==="
        (
            cd "$dir"
            timeout "$TIMEOUT" codex -q "$prompt" --full-auto 2>"trace.log" || echo "TIMEOUT/ERROR"
        )
        echo "  Done: $dir"
    fi

    # Claude Code
    if $HAVE_CLAUDE && should_run_mode "claude"; then
        dir="$RESULTS_DIR/${case_name}/claude-direct"
        mkdir -p "$dir"
        echo "=== $case_name / claude direct ==="
        (
            cd "$dir"
            timeout "$TIMEOUT" claude -p "$prompt" 2>"trace.log" || echo "TIMEOUT/ERROR"
        )
        echo "  Done: $dir"
    fi
done

echo ""
echo "=========================================="
echo " 执行完成"
echo " 结果: $RESULTS_DIR"
echo ""
echo " 下一步:"
echo "   1. 手动完成 makewand 交互式测试"
echo "   2. 运行评分: ./benchmarks/score.sh $RESULTS_DIR"
echo "=========================================="
