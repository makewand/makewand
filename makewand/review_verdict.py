"""Format a review verdict for callers without reparsing a finalized decision."""

from typing import Any, Dict, List, Optional

from makewand.review_contract import REVIEW_PASSED, REVIEW_UNVERIFIED, _VERDICT_ANY_RE, evaluate_review_verdict


def extract_review_verdict_dict(
    review_text: str, nonce: Optional[str] = None, *, verdict: Optional[Dict[str, Any]] = None
) -> Dict[str, Any]:
    """
    Extract structured review verdict and defects list from review output.
    """
    if verdict is None:
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
