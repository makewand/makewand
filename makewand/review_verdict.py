"""
Review verdict resolution, artifact persistence, and patch parsimony.

Decoupled from makewand/orchestrator.py to provide modular, testable
verdict parsing, defect extraction, autofix prompt synthesis, and
undelivered patch persistence.
"""

from typing import Dict, Any, List, Optional, Tuple

from makewand.config import c, COLOR_YELLOW, COLOR_RED, COLOR_GREEN
from makewand.review_contract import REVIEW_PASSED, REVIEW_UNVERIFIED, evaluate_review_verdict, canonical_verdict_line, strip_verdict_lines, build_verdict_followup_prompt, reviewer_prompt_limit, reviewer_receives_whole_prompt, _VERDICT_ANY_RE


def extract_review_verdict_dict(review_text: str, nonce: Optional[str] = None) -> Dict[str, Any]:
    """
    Extract structured review verdict and defects list from review output.
    """
    verdict = evaluate_review_verdict(review_text, nonce=nonce)
    passed = verdict["status"] == REVIEW_PASSED
    defects: List[str] = list(verdict["defects"])

    if not passed and not defects and review_text:
        for line in review_text.splitlines():
            l_strip = line.strip()
            if _VERDICT_ANY_RE.search(l_strip):
                continue
            if any(tag in l_strip.upper() for tag in ["[P0]", "[P1]", "[P2]", "P0:", "P1:", "P2:", "CRITICAL", "DEFECT"]):
                defects.append(l_strip[:200])
                if len(defects) >= 5:
                    break
    if verdict["status"] == REVIEW_UNVERIFIED and verdict["reason"] and verdict["reason"] not in defects:
        defects.insert(0, f"UNVERIFIED: {verdict['reason']}")

    return {
        "pass": passed,
        "defects": defects,
        "verdict_status": verdict["status"],
    }


def resolve_review_verdict(
    review_text: Optional[str],
    engine: Optional[str],
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "deep",
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    quiet: bool = False,
    dispatch_fn: Optional[Any] = None,
    stage_call_fn: Optional[Any] = None,
    nonce: Optional[str] = None,
) -> Tuple[str, Dict[str, Any]]:
    """
    Evaluates a review; if the structured verdict is missing or malformed, asks the same reviewer
    exactly once for the verdict line only. Returns (review_text_for_downstream, verdict).
    """
    text = review_text or ""
    verdict = evaluate_review_verdict(text, nonce=nonce)
    if verdict["status"] != REVIEW_UNVERIFIED or not text.strip() or not engine or timeout <= 0:
        return text, verdict
    tag = f"MAKEWAND_VERDICT_{nonce}" if nonce else "MAKEWAND_VERDICT"
    if not quiet:
        print(c(f"⚠ [Makewand Verdict] {engine.upper()} 的审查缺少有效裁决 ({verdict['reason']})，追问一次仅要求输出 {tag} 裁决行...", COLOR_YELLOW))

    prompt = build_verdict_followup_prompt(text, verdict["reason"], nonce=nonce)
    if not reviewer_receives_whole_prompt(engine, prompt):
        # The verdict must rest on the reviewer's complete review, not on part of it.
        verdict = dict(verdict, reason=f"{verdict['reason']}；追问提示超过 {engine.upper()} 可完整接收的 "
                                       f"{reviewer_prompt_limit(engine)} 字节上限，未追问")
        if not quiet:
            print(c(f"❌ [Makewand Verdict] {verdict['reason']}，判定为 UNVERIFIED。", COLOR_RED))
        return text, verdict
    if dispatch_fn is not None and stage_call_fn is not None:
        res = stage_call_fn("review", dispatch_fn, engine, prompt, engine=engine,
                            cwd=cwd, timeout=timeout, tier=tier, stream=False, readonly=True, repo_root=repo_root, repo_trust=repo_trust)
    elif dispatch_fn is not None:
        res = dispatch_fn(engine, prompt, cwd=cwd, timeout=timeout, tier=tier, stream=False, readonly=True, repo_root=repo_root, repo_trust=repo_trust)
    else:
        from makewand.orchestrator import dispatch_task, _stage_call
        res = _stage_call("review", dispatch_task, engine, prompt, engine=engine,
                          cwd=cwd, timeout=timeout, tier=tier, stream=False, readonly=True, repo_root=repo_root, repo_trust=repo_trust)

    from makewand.workflow import is_startup_environment_failure
    if is_startup_environment_failure(res):
        return text, dict(verdict, execution_status="UNVERIFIED",
                          error_kind="startup_environment", reason=str(res.error))
    if getattr(res, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
        return text, dict(verdict, execution_status=res.status,
                          reason="审查补充裁决结果未确定或预算已耗尽，停止后续派发")
    followup = res[1] if isinstance(res, (tuple, list)) and len(res) == 3 and res[0] else None
    followup_verdict = evaluate_review_verdict(followup, nonce=nonce)
    if followup_verdict["status"] == REVIEW_UNVERIFIED:
        verdict = dict(verdict)
        verdict["reason"] = f"{verdict['reason']}；追问后仍未获得有效裁决 ({followup_verdict['reason']})"
        if not quiet:
            print(c("❌ [Makewand Verdict] 追问后仍未获得有效裁决，判定为 UNVERIFIED。", COLOR_RED))
        return text, verdict
    combined = f"{strip_verdict_lines(text).rstrip()}\n\n【审查者追问补充裁决】\n{canonical_verdict_line(followup_verdict, nonce=nonce)}"
    if not quiet:
        print(c(f"✔ [Makewand Verdict] 已获得 {engine.upper()} 的补充裁决: {canonical_verdict_line(followup_verdict, nonce=nonce)}", COLOR_GREEN))
    return combined, evaluate_review_verdict(combined, nonce=nonce)


