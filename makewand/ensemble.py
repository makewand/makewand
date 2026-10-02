"""
Multi-Model Ensemble & Brain Trust Orchestrator.

Enables concurrent panel reviews, manuscript peer-review simulations,
architecture critiques, and multi-model consensus verification across
all active frontier AI subscriptions (Claude, Codex, Muse, Grok, AGY, Local).
"""

import os
import sys
import time
import json
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Dict, Any, List, Optional, Tuple, Union

from makewand.config import (
    c, COLOR_BLUE, COLOR_CYAN, COLOR_GREEN, COLOR_PURPLE,
    COLOR_RED, COLOR_YELLOW, COLOR_BOLD, is_provider_enabled,
    has_subscription_configured, has_api_configured
)
from makewand.discovery import get_provider_model_tier
from makewand.review_verdict import extract_review_verdict_dict


FRONTIER_ENSEMBLE_PROVIDERS: List[str] = ["claude", "codex", "agy", "muse", "grok"]
ALL_ENSEMBLE_PROVIDERS: List[str] = ["claude", "codex", "agy", "muse", "grok", "local"]


def resolve_ensemble_providers(
    requested: Optional[Union[str, List[str]]] = None,
    local_only: bool = False
) -> List[str]:
    """
    Resolves and validates the active list of providers for an ensemble panel.
    Filters to configured, enabled, and healthy providers.
    """
    if local_only:
        return ["local"]

    if requested:
        if isinstance(requested, str):
            req_parts = [p.strip().lower() for p in requested.split(",") if p.strip()]
            if "all" in req_parts:
                candidates = list(ALL_ENSEMBLE_PROVIDERS)
            elif requested.strip().lower() in ("frontier", "default"):
                candidates = list(FRONTIER_ENSEMBLE_PROVIDERS)
            else:
                candidates = req_parts
        else:
            req_parts = [str(p).strip().lower() for p in requested if str(p).strip()]
            if "all" in req_parts:
                candidates = list(ALL_ENSEMBLE_PROVIDERS)
            else:
                candidates = req_parts
    else:
        candidates = list(FRONTIER_ENSEMBLE_PROVIDERS)

    # Validate availability of each candidate
    selected: List[str] = []
    from makewand.health import load_status_cache
    cache = load_status_cache()

    for p in candidates:
        if not is_provider_enabled(p):
            continue
        if p in ("local", "ollama"):
            from makewand.providers.local import is_local_model_available
            avail, _, _ = is_local_model_available()
            if not avail:
                continue
            if p not in selected:
                selected.append(p)
            continue
        # Check if subscription or API is available
        if has_subscription_configured(p) or has_api_configured(p):
            # Check if limited
            p_status = cache.get(p, {}).get("status")
            if p_status != "limited" or has_api_configured(p):
                if p not in selected:
                    selected.append(p)

    if not selected:
        # Fallback to local or agy
        if has_subscription_configured("agy") or has_api_configured("agy"):
            selected = ["agy"]
        else:
            selected = ["local"]

    return selected


def extract_summary_bullet(text: Optional[str], max_len: int = 140) -> str:
    """Extracts a concise one-line finding or verdict from model response."""
    if not text or not text.strip():
        return "无输出"

    # 1. Try structured verdict first if MAKEWAND_VERDICT is present
    if "MAKEWAND_VERDICT" in text:
        verdict = extract_review_verdict_dict(text)
        if verdict.get("pass"):
            return "评审通过 (Pass: True, 零关键缺陷)"
        defects = [d for d in verdict.get("defects", []) if not d.startswith("UNVERIFIED:")]
        if defects:
            return f"发现缺陷 ({len(defects)}项): {'; '.join(defects[:2])}"[:max_len]

    # 2. Look for verdict lines like Accept, LGTM, 审稿结论
    lines = [l.strip() for l in text.splitlines() if l.strip()]
    for l in lines:
        l_clean = l.lstrip("#*-> ").strip()
        if any(kw in l_clean for kw in [
            "Accept as is", "Accept with Minor", "Major Revision", "Reject",
            "审稿结论", "最终裁决", "总评", "评审结论", "LGTM", "Recommendation:"
        ]):
            return l_clean[:max_len]

    # 3. Fallback to first non-heading sentence
    for l in lines:
        if not l.startswith("#") and len(l) > 10:
            return l[:max_len]

    return lines[0][:max_len] if lines else "完成"


def format_ensemble_matrix(
    results: Dict[str, Dict[str, Any]],
    effort: str = "high",
    tier: str = "deep"
) -> str:
    """Generates a clean Markdown synthesis table comparing all reviewer outcomes."""
    lines = [
        "# 多模型智囊团联合评审汇总矩阵 (Makewand Ensemble Review Matrix)",
        "",
        f"- **评审时间**: {time.strftime('%Y-%m-%d %H:%M:%S')}",
        f"- **思考深度 (Reasoning Effort)**: `{effort.upper()}` | **模型档位**: `{tier}`",
        f"- **参与专家数**: {len(results)} 位独立 AI 评审专家",
        "",
        "| 评审模型专家 | 状态 | 耗时 | 核心裁决与关键要点 | 独立报告归档 |",
        "|---|---|---|---|---|"
    ]

    for prov, r in results.items():
        name = r.get("model_display") or prov.upper()
        status_icon = "✅ 成功" if r.get("ok") else "❌ 失败"
        dur = f"{r.get('duration', 0.0):.1f}s"
        summary = r.get("summary", "无输出").replace("|", "\\|")
        out_file = r.get("output_file")
        file_link = f"`{out_file}`" if out_file else "-"
        lines.append(f"| **{name}** | {status_icon} | {dur} | {summary} | {file_link} |")

    lines.append("")
    lines.append("---")
    lines.append("### 专家独立评审意见概览")
    lines.append("")

    for prov, r in results.items():
        name = r.get("model_display") or prov.upper()
        lines.append(f"#### 专家: {name}")
        if r.get("ok"):
            preview = (r.get("output") or "").strip()
            if len(preview) > 500:
                preview = preview[:500] + "\n\n*(后续详情请参见完整独立归档文件)*"
            lines.append(preview)
        else:
            lines.append(f"> ❌ 执行失败或超时: {r.get('error') or '未知错误'}")
        lines.append("")

    return "\n".join(lines)


def run_ensemble(
    prompt: str,
    providers: Optional[Union[str, List[str]]] = None,
    cwd: Optional[str] = None,
    timeout: int = 900,
    tier: str = "deep",
    effort: Optional[str] = "high",
    output_dir: Optional[str] = None,
    prefix: str = "review",
    matrix: bool = True,
    stream: bool = False,
    local_only: bool = False,
    repo_trust: str = "trusted",
) -> Dict[str, Any]:
    """
    Executes an ensemble panel review across multiple AI models concurrently.
    All models run read-only without modifying files, returning independent reviews
    and optionally generating a comparison matrix.
    """
    from makewand.orchestrator import dispatch_task

    selected = resolve_ensemble_providers(providers, local_only=local_only)
    if not selected:
        return {
            "timestamp": time.time(),
            "ok": False,
            "error": "未找到任何可用的 AI 评审引擎，请确认订阅登录状态或配置 API 密钥。",
            "results": {},
            "summary_matrix": "",
        }

    work_dir = os.path.abspath(cwd or os.getcwd())
    effort_str = (effort or "high").lower()

    if output_dir:
        abs_out_dir = os.path.abspath(output_dir if os.path.isabs(output_dir) else os.path.join(work_dir, output_dir))
        os.makedirs(abs_out_dir, exist_ok=True)
    else:
        abs_out_dir = None

    print(c("\n╔══════════════════════════════════════════════════════════════════════════════╗", COLOR_BOLD + COLOR_BLUE), file=sys.stderr)
    print(c("║  🎯 [Makewand Ensemble] 多模型智囊团联合评审启动                            ║", COLOR_BOLD + COLOR_BLUE), file=sys.stderr)
    print(c("╚══════════════════════════════════════════════════════════════════════════════╝", COLOR_BOLD + COLOR_BLUE), file=sys.stderr)
    print(c(f"• 选定评审专家阵容: {', '.join([p.upper() for p in selected])}", COLOR_CYAN), file=sys.stderr)
    print(c(f"• 思考深度: {effort_str.upper()} | 模型档位: {tier} | 单模型超时: {timeout}s", COLOR_CYAN), file=sys.stderr)
    if abs_out_dir:
        print(c(f"• 评审归档目录: {abs_out_dir}", COLOR_CYAN), file=sys.stderr)
    print(c("• 安全隔离: 100% 只读分析模式 (Read-Only)，安全避开代码踩踏与文件修改。\n", COLOR_GREEN), file=sys.stderr)

    results: Dict[str, Dict[str, Any]] = {}

    def _invoke_single_expert(prov: str) -> Tuple[str, bool, Optional[str], Optional[str], float, str]:
        start = time.time()
        # Resolve exact display model name and target model ID
        resolved_model = None
        try:
            m_info = get_provider_model_tier(prov, tier)
            resolved_model = m_info.get("model")
            model_display = f"{prov.upper()} ({resolved_model or 'default'})"
        except Exception:
            model_display = prov.upper()

        print(c(f"[{time.strftime('%H:%M:%S')}] >>> 启动专家评审: {model_display} ...", COLOR_BLUE), file=sys.stderr)

        ok, out, err = dispatch_task(
            prov,
            prompt,
            cwd=work_dir,
            timeout=timeout,
            tier=tier,
            model=resolved_model,
            effort=effort_str,
            readonly=True,
            repo_trust=repo_trust,
            stream=stream,
        )
        duration = round(time.time() - start, 2)
        status_tag = c("完成", COLOR_GREEN) if ok else c("失败", COLOR_RED)
        print(c(f"[{time.strftime('%H:%M:%S')}] <<< 专家评审 {model_display} {status_tag} (耗时: {duration}s)", COLOR_CYAN), file=sys.stderr)
        return prov, ok, out, err, duration, model_display

    with ThreadPoolExecutor(max_workers=min(len(selected), 8)) as executor:
        futures = {executor.submit(_invoke_single_expert, p): p for p in selected}
        for fut in as_completed(futures):
            prov, ok, out, err, duration, model_display = fut.result()
            output_file_path = None
            if abs_out_dir and ok and out:
                target_fname = f"{prefix}_{prov}.md"
                target_fpath = os.path.join(abs_out_dir, target_fname)
                try:
                    with open(target_fpath, "w", encoding="utf-8") as f:
                        f.write(out)
                    output_file_path = target_fpath
                except Exception as e:
                    print(c(f"⚠ 无法写入归档文件 {target_fpath}: {e}", COLOR_YELLOW), file=sys.stderr)

            summary = extract_summary_bullet(out) if ok else (err or "调用失败")
            results[prov] = {
                "ok": ok,
                "output": out,
                "error": err,
                "duration": duration,
                "model_display": model_display,
                "summary": summary,
                "output_file": output_file_path,
            }

    matrix_text = format_ensemble_matrix(results, effort=effort_str, tier=tier) if matrix else ""
    matrix_file_path = None
    if abs_out_dir and matrix_text:
        matrix_fname = f"{prefix}_matrix.md"
        matrix_file_path = os.path.join(abs_out_dir, matrix_fname)
        try:
            with open(matrix_file_path, "w", encoding="utf-8") as f:
                f.write(matrix_text)
            print(c(f"\n📊 评审汇总对比矩阵已落盘至: {matrix_file_path}", COLOR_GREEN + COLOR_BOLD), file=sys.stderr)
        except Exception as e:
            print(c(f"⚠ 无法写入汇总对比矩阵文件 {matrix_file_path}: {e}", COLOR_YELLOW), file=sys.stderr)

    all_succeeded = all(r["ok"] for r in results.values())
    any_succeeded = any(r["ok"] for r in results.values())

    return {
        "timestamp": time.time(),
        "ok": any_succeeded,
        "all_succeeded": all_succeeded,
        "providers": selected,
        "results": results,
        "summary_matrix": matrix_text,
        "matrix_file": matrix_file_path,
    }
