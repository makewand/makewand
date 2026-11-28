"""Model constraints for explicitly high-risk reviews."""
import re
from dataclasses import dataclass
from typing import Optional

from makewand.discovery import _split_agy_suffix
from makewand.routing_catalog import MIN_CURRENT_MODEL_VERSIONS, PROVIDER_MODEL_FAMILIES, _tier_class, candidates

HIGH_EFFORTS = frozenset(("high", "xhigh", "max", "ultra"))
NATIVE_PROVIDERS = {family: provider for provider, family in PROVIDER_MODEL_FAMILIES.items()}
VENDOR_FAMILIES = {
    "anthropic": {"claude"},
    "openai": {"gpt"},
    "google": {"gemini", "gemma"},
    "xai": {"grok"},
}
CLAUDE_SERIES = r"haiku|sonnet|opus|fable|mythos"
VERSION = r"[0-9]{1,2}(?:[.-][0-9]{1,2}){0,2}"
VERSION_END = r"(?=$|[-._:\[])"
MODEL_FAMILY = re.compile(
    r"^(gpt|gemini|gemma|grok|muse|qwen|llama|deepseek|glm|kimi)(?=$|[-._0-9])"
)


@dataclass(frozen=True)
class ReviewModelIdentity:
    family: str
    model: str
    series: str = ""
    version: tuple = ()


def review_model_identity(provider, model) -> Optional[ReviewModelIdentity]:
    """Interpret the underlying model while leaving the provider's ID intact."""
    name = (model or "").lower()
    if provider == "agy":
        name = _split_agy_suffix(name)[0]
    vendor, separator, name = name.rpartition("/")
    if separator and vendor not in VENDOR_FAMILIES:
        return None
    # Claude has used both product-before-version and version-before-product.
    historical = re.match(rf"^claude-({VERSION})-({CLAUDE_SERIES}){VERSION_END}", name)
    if historical:
        version, series = historical.groups()
        name = f"claude-{series}-{version}" + name[historical.end():]
    claude = re.match(rf"^(?:claude-)?({CLAUDE_SERIES}){VERSION_END}", name)
    muse = re.match(r"^muse-([a-z]+)(?=-|$)", name)
    series = ""
    if claude:
        family, series = "claude", claude[1]
        version_text = name[claude.end():].removeprefix("-")
        if not name.startswith("claude-"):
            name = "claude-" + name
    elif muse:
        family, series = "muse", muse[1]
        version_text = name[muse.end():].removeprefix("-")
    else:
        if re.match(r"^google-(?:gemini-|[0-9])", name):
            name = "gemini-" + name.removeprefix("google-").removeprefix("gemini-")
        match = MODEL_FAMILY.match(name)
        if match:
            family = match[1]
            version_text = name[match.end():].removeprefix("-")
        elif name == "claude" or re.match(r"^(?:o[134]|codex)(?=$|-)", name):
            family = "claude" if name == "claude" else "gpt"
            version_text = ""
        else:
            return None
    if separator and family not in VENDOR_FAMILIES[vendor]:
        return None
    version_match = re.match(rf"^({VERSION}){VERSION_END}", version_text)
    version = ()
    if version_match:
        parts = tuple(int(part) for part in re.split(r"[.-]", version_match[1]))
        version = parts + (0,) * (3 - len(parts))
    return ReviewModelIdentity(family, name, series, version)


def review_model_family(provider, model):
    identity = review_model_identity(provider, model)
    return identity.family if identity else None


def review_family(name):
    if not name:
        raise ValueError("高风险审查需要指定实际实现方")
    family = PROVIDER_MODEL_FAMILIES.get(name.lower()) or review_model_family(None, name)
    if family is None:
        raise ValueError("无法确认实现方或排除项的模型家族")
    return family


@dataclass(frozen=True)
class ReviewPolicy:
    author_family: str
    excluded_families: frozenset
    allow_same_family: bool = False

    @classmethod
    def from_options(cls, author, excludes, allow_same_family):
        return cls(review_family(author), frozenset(review_family(name) for name in excludes), allow_same_family)

    def check_family(self, provider, model):
        family = review_model_family(provider, model)
        if provider == "grok" or family == "grok":
            return "高风险审查排除 Grok"
        if not family or family in self.excluded_families:
            return "模型家族未知或已排除"
        if family == self.author_family and not self.allow_same_family:
            return "与实现方同模型家族，需要明确批准回退"
        return None


def review_models(discovered):
    """Newest discovered strong generation per model lineage, including proxies."""
    rows = []
    newest = {}
    for row in candidates(3, full=True, discovered=discovered):
        if row.effort not in HIGH_EFFORTS or row.effort not in discovered.get(row.provider, {}).get(row.model, ()):
            continue
        identity = review_model_identity(row.provider, row.model)
        if not identity or not identity.version or identity.version == (0, 0, 0):
            continue
        version_key = "muse-spark" if identity.family == "muse" and identity.series == "spark" else identity.family
        if identity.version < MIN_CURRENT_MODEL_VERSIONS.get(version_key, (0, 0, 0)):
            continue
        native = NATIVE_PROVIDERS.get(identity.family, row.provider)
        if _tier_class(native, identity.model, row.effort) != 3:
            continue
        group = (identity.family, identity.series)
        version = identity.version
        newest[group] = max(newest.get(group, version), version)
        rows.append((row, group, version))
    return {(row.provider, row.model, row.effort) for row, group, version in rows if version == newest[group]}


def validate_review_dispatch(policy, provider, model, effort, discovered):
    """The effort this high-risk review launches with, or ValueError saying what to change.

    Other providers' adapters translate the requested effort into what their
    CLI accepts (Claude runs "ultra" as "max", Codex clamps to the levels the
    model lists). The translated value is returned, so the launch check
    compares like with like instead of refusing a "changed" effort. A high
    effort that would launch below the high efforts is refused here, as the
    launch check refused it before, with the reason.
    """
    if not model and provider != "grok":
        # Without it the adapter picks a default model and the family check below cannot name one.
        raise ValueError("高风险审查需要用 --model 指定精确型号（route review hard --risk high 给出的命令里带型号）")
    if provider == "agy":
        suffix = _split_agy_suffix(model or "")[1]
        # AGY carries its effective effort in the model ID.
        if effort and effort != suffix:
            raise ValueError("AGY 的推理档必须与模型 ID 后缀一致")
        effort = suffix
    reason = policy.check_family(provider, model)
    if reason:
        raise ValueError(reason)
    if not effort:
        # Without it the adapter picks a default and the launch check would refuse a "changed" effort.
        raise ValueError("高风险审查需要明确的推理档：用 --effort 指定（AGY 用带推理档后缀的模型 ID，如 gemini-4-argon-high）")
    if provider != "agy":
        from makewand.discovery import normalize_provider_effort
        launched = normalize_provider_effort(provider, effort, model=model) or effort
        if effort in HIGH_EFFORTS and launched not in HIGH_EFFORTS:
            raise ValueError(f"{provider} 的 {model} 最高只提供推理档 {launched}，达不到所要求的 {effort}；"
                             "高风险审查请换一个有高推理档的型号")
        effort = launched
    return effort


def review_currency_reminder(provider, model, effort, discovered):
    """Text for a model the local catalog does not name as the newest strong tier, else None.

    A reminder, not a refusal: the catalog is a cache of the provider's own
    files, and a stale or reshaped one kept every Claude model out of
    high-risk review (2026-11-27).
    """
    try:
        if review_model_is_current(provider, model, effort, discovered):
            return None
    except Exception:  # noqa: BLE001 - an unreadable catalog proves nothing about the model
        pass
    return (f"本机目录未把 {model or '默认模型'} 列为最新强档，或缺少该推理档的证据；这一条只提醒，不因此拦截。"
            "可运行 route review hard --risk high 核对当前推荐")


def review_model_is_current(provider, model, effort, discovered):
    if provider == "agy":
        effort = _split_agy_suffix(model or "")[1]
    return (provider, model, effort) in review_models(discovered)
