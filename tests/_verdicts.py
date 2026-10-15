"""Nonce-bound verdict lines for fake reviewers and race judges.

Every review prompt asks for ``MAKEWAND_VERDICT_<nonce>:`` and every race judge
prompt for ``MAKEWAND_RACE_VERDICT_<nonce>:``, with a random per-task nonce; an
un-nonced verdict line never counts, in tests as in production (it could be a
verdict echoed from the reviewed content). A fake reviewer therefore answers
with the tag its prompt asked for: ``nonced(prompt, answer)`` binds the plain
``MAKEWAND_VERDICT:`` / ``MAKEWAND_RACE_VERDICT:`` tags of a canned answer to that
nonce, ``nonced_result(result, prompt)`` does so for a whole (ok, text, error)
result, and ``answer_with_nonce(fake)`` for every answer of a fake dispatch_task
or provider adapter (a function, one canned result, or a list of canned results
as for ``side_effect``). A test of the legacy un-nonced format passes canned text
unchanged and asks for the legacy reading explicitly (``allow_legacy=True``).
"""

import re
from typing import Any, Optional

# The tag a prompt asks for is named in its output contract ("…必须以 MAKEWAND_VERDICT_<n>: 开头",
# "…该行以 MAKEWAND_VERDICT_<n>: 开头", "…输出 MAKEWAND_RACE_VERDICT_<n>: …"), never inside a
# candidate diff, which may quote any tag.
_ASKED_TAG_RE = re.compile(r"以 MAKEWAND_(?:RACE_)?VERDICT_([0-9a-f]{8}): 开头|输出 MAKEWAND_RACE_VERDICT_([0-9a-f]{8}):")
_PLAIN_TAG_RE = re.compile(r"MAKEWAND_(RACE_)?VERDICT(?=[ \t*_`]*[:：])")


def prompt_nonce(prompt: Optional[str]) -> Optional[str]:
    """The nonce a review or judge prompt asks its verdict line to carry, or None."""
    match = _ASKED_TAG_RE.search(prompt or "")
    return (match.group(1) or match.group(2)) if match else None


def nonced(prompt: Optional[str], answer: Optional[str]) -> Optional[str]:
    """answer with its plain verdict tags bound to the nonce prompt asks for (unchanged
    when the prompt asks for none, e.g. a coding prompt)."""
    nonce = prompt_nonce(prompt)
    if not nonce or not isinstance(answer, str):
        return answer
    return _PLAIN_TAG_RE.sub(lambda match: f"MAKEWAND_{match.group(1) or ''}VERDICT_{nonce}", answer)


def _prompt_of(args, kwargs) -> Optional[str]:
    for value in (kwargs.get("prompt"), *args, *kwargs.values()):
        if isinstance(value, str) and prompt_nonce(value):
            return value
    return None


def nonced_result(result: Any, prompt: Optional[str]) -> Any:
    """An (ok, text, error) result, or ExecutionResult, whose text is nonced(prompt, text)."""
    if prompt is None or not isinstance(result, tuple) or len(result) != 3 or not isinstance(result[1], str):
        return result
    answer = nonced(prompt, result[1])
    if answer == result[1]:
        return result
    from makewand.execution_contract import ExecutionResult
    if isinstance(result, ExecutionResult):
        metadata = {key: value for key, value in result._metadata.items() if key != "exit_code"}
        return ExecutionResult(result[0], answer, result[2], **metadata)
    return (result[0], answer, result[2])


def answer_with_nonce(fake):
    """A fake dispatch_task / execute_*_task whose verdict lines carry the nonce of the
    prompt it was given. ``fake`` is a function, one canned (ok, text, error) result
    given for every call (as a return_value would be), or a list of canned results
    consumed in order (as a side_effect list would be)."""
    if callable(fake):
        def respond(*args, **kwargs):
            return nonced_result(fake(*args, **kwargs), _prompt_of(args, kwargs))
    elif isinstance(fake, tuple):
        def respond(*args, **kwargs):
            return nonced_result(fake, _prompt_of(args, kwargs))
    else:
        results = iter(fake)

        def respond(*args, **kwargs):
            result = next(results)
            if isinstance(result, BaseException) or (isinstance(result, type) and issubclass(result, BaseException)):
                raise result
            return nonced_result(result, _prompt_of(args, kwargs))
    return respond
