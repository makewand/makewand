"""
Provider CLI messages: the Go router and makewand/health.py reach the same
verdict on the same CLI output.

Both read one catalog, makewand/cli_message_catalog.json: router/cli.go embeds
the byte-identical copy router/cli_message_catalog.json and ports
find_cli_messages (findCLIMessages). tests/testdata/cli_message_parity.json is
the shared corpus with what health.py finds in each case; this test checks
health.py against it, router/cli_message_parity_test.go checks the Go port
against the same file.

After a change to the catalog or to health.py's line selection, copy the
catalog to router/ and regenerate the expectations:

    cp makewand/cli_message_catalog.json router/cli_message_catalog.json
    UPDATE_CLI_PARITY=1 python3 -m pytest tests/test_cli_message_parity.py
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import hashlib
import json
import os
import unittest
from pathlib import Path

import makewand.health as health

REPO_ROOT = Path(__file__).resolve().parent.parent
CATALOG_FILE = REPO_ROOT / "makewand" / "cli_message_catalog.json"
GO_CATALOG_FILE = REPO_ROOT / "router" / "cli_message_catalog.json"
CORPUS_FILE = Path(__file__).resolve().parent / "testdata" / "cli_message_parity.json"
UPDATE_ENV = "UPDATE_CLI_PARITY"  # not MAKEWAND_*: _isolation scrubs those
MODES = ("merged", "stderr", "stdout", "apart")

CORPUS_DOC = [
    "Provider CLI message parity corpus: makewand/health.py find_cli_messages and router/cli.go findCLIMessages "
    "must find the same messages on the same lines. Read by tests/test_cli_message_parity.py and "
    "router/cli_message_parity_test.go; after a change to the catalog or to health.py, regenerate 'expect' "
    "with UPDATE_CLI_PARITY=1 python3 -m pytest tests/test_cli_message_parity.py.",
    "A string field is a string, or a list of strings and [unit, count] pairs to concatenate.",
    "text is read as one merged stream (merged), as stderr beside an empty stdout (stderr: what the router's "
    "formatCLIExecutionError reads) and as stdout beside an empty stderr (stdout); stdout and stderr as streams "
    "captured apart (apart). prompt is the prompt as sent.",
    "want (Q: a CLI's own quota or rate refusal, N: task content) and prov label the corpus a case came from "
    "(the round-2 verifier, earlier Go and Python tests, engine edge cases); they are not expectations.",
    "expect.<provider>.<modes>.<kind> is the normalized line on which health.py finds a message of that kind, "
    "for every kind of the provider's catalog entry; <modes> joins the modes with the same findings by ','. "
    "A provider or mode that is absent finds nothing.",
]


def expand(value):
    """A corpus string field: a string, or strings and [unit, count] pairs to concatenate."""
    if value is None or isinstance(value, str):
        return value
    return "".join(part if isinstance(part, str) else part[0] * part[1] for part in value)


def compact(text):
    """The inverse of expand for long runs of a short unit."""
    if len(text) < 200:
        return text
    parts, literal, index = [], "", 0
    while index < len(text):
        best = None
        for size in range(1, 7):
            unit = text[index:index + size]
            if len(unit) < size:
                break
            count = 1
            while text.startswith(unit, index + count * size):
                count += 1
            if count * size >= 40 and (best is None or count * size > best[1] * len(best[0])):
                best = (unit, count)
        if best:
            if literal:
                parts.append(literal)
                literal = ""
            parts.append([best[0], best[1]])
            index += len(best[0]) * best[1]
        else:
            literal += text[index]
            index += 1
    if literal:
        parts.append(literal)
    return text if parts == [text] else parts


def provider_kinds(provider):
    return tuple(sorted({kind.split(".")[0] for kind in health.CLI_MESSAGE_CATALOG[provider]}))


def classify(case):
    """What health.py finds in a corpus case: {provider: {mode: {kind: line}}}, non-empty only."""
    text, prompt = expand(case.get("text")), expand(case.get("prompt"))
    found = {}
    for provider in sorted(health.CLI_MESSAGE_CATALOG):
        kinds = provider_kinds(provider)
        modes = {}
        if text is not None:
            modes["merged"] = health.find_cli_messages(provider, kinds, text, prompt=prompt)
            modes["stderr"] = health.find_cli_messages(provider, kinds, "", text, prompt=prompt)
            modes["stdout"] = health.find_cli_messages(provider, kinds, text, "", prompt=prompt)
        if "stdout" in case or "stderr" in case:
            modes["apart"] = health.find_cli_messages(provider, kinds, expand(case.get("stdout")) or "",
                                                      expand(case.get("stderr")) or "", prompt=prompt)
        modes = {mode: dict(sorted(lines.items())) for mode, lines in modes.items() if lines}
        if modes:
            found[provider] = modes
    return found


def encode_expectations(found):
    """{provider: {"mode,mode": {kind: line}}}: modes with the same findings share an entry."""
    encoded = {}
    for provider, modes in found.items():
        groups = []
        for mode in MODES:
            if mode in modes:
                for group in groups:
                    if group[1] == modes[mode]:
                        group[0].append(mode)
                        break
                else:
                    groups.append(([mode], modes[mode]))
        encoded[provider] = {",".join(names): {kind: compact(line) for kind, line in lines.items()}
                             for names, lines in groups}
    return encoded


def decode_expectations(expect):
    decoded = {}
    for provider, groups in (expect or {}).items():
        modes = decoded.setdefault(provider, {})
        for names, lines in groups.items():
            for mode in names.split(","):
                modes[mode] = {kind: expand(line) for kind, line in lines.items()}
    return {provider: {mode: modes[mode] for mode in MODES if mode in modes} for provider, modes in decoded.items()}


def catalog_sha256():
    return hashlib.sha256(CATALOG_FILE.read_bytes()).hexdigest()


def write_corpus(corpus):
    """The corpus as JSON with one case per line."""
    head = json.dumps({key: value for key, value in corpus.items() if key != "cases"}, ensure_ascii=True, indent=1)
    cases = ",\n".join("  " + json.dumps(case, ensure_ascii=True, separators=(",", ":")) for case in corpus["cases"])
    with open(CORPUS_FILE, "w", encoding="ascii") as handle:
        handle.write(head[:-2] + ',\n "cases": [\n' + cases + "\n ]\n}\n")


def regenerate():
    corpus = json.loads(CORPUS_FILE.read_text(encoding="utf-8"))
    corpus["_doc"] = CORPUS_DOC
    corpus["catalog_sha256"] = catalog_sha256()
    for case in corpus["cases"]:
        case.pop("expect", None)
        expect = encode_expectations(classify(case))
        if expect:
            case["expect"] = expect
    write_corpus({"_doc": corpus["_doc"], "catalog_sha256": corpus["catalog_sha256"], "cases": corpus["cases"]})


class TestCLIMessageParityCorpus(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.environ.get(UPDATE_ENV) == "1":
            regenerate()
        cls.corpus = json.loads(CORPUS_FILE.read_text(encoding="utf-8"))

    def test_go_router_embeds_the_catalog_byte_for_byte(self):
        self.assertTrue(GO_CATALOG_FILE.is_file(), f"{GO_CATALOG_FILE} is missing")
        self.assertEqual(GO_CATALOG_FILE.read_bytes(), CATALOG_FILE.read_bytes(),
                         "router/cli_message_catalog.json must be a copy of makewand/cli_message_catalog.json")

    def test_corpus_expectations_come_from_this_catalog(self):
        self.assertEqual(self.corpus["catalog_sha256"], catalog_sha256(),
                         f"the catalog changed: regenerate with {UPDATE_ENV}=1 (see the module docstring)")

    def test_corpus_holds_the_round_2_verifier_cases(self):
        ids = [case["id"] for case in self.corpus["cases"]]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual(len([cid for cid in ids if cid.startswith(("v1-", "v2-"))]), 134 + 10 + 7)

    def test_health_finds_what_the_corpus_expects(self):
        for case in self.corpus["cases"]:
            with self.subTest(case=case["id"]):
                self.assertEqual(classify(case), decode_expectations(case.get("expect")))


if __name__ == "__main__":
    unittest.main()
