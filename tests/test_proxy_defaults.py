"""Offline proxy contract and fake CLI/API coverage shared with Go."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import subprocess
import sys
from pathlib import Path
from unittest.mock import patch

import pytest
from makewand import config
from makewand.proxy import PROXY_KEYS, proxy_environment, proxy_status, urlopen
from makewand.sandbox import provider_sandbox_launch, wrap_bwrap

CASES = json.loads((Path(__file__).parent / "fixtures/proxy_contract.json").read_text())


@pytest.fixture
def isolated_config(tmp_path, monkeypatch):
    for key in (*PROXY_KEYS, "NO_PROXY", "no_proxy", "MAKEWAND_DEFAULT_PROXY", "MAKEWAND_DEFAULT_NO_PROXY", "MAKEWAND_GEMINI_USE_PROXY", "MAKEWAND_GEMINI_BYPASS_PROXY"):
        monkeypatch.delenv(key, raising=False)
    path = tmp_path / "config.json"
    monkeypatch.setattr(config, "CONFIG_FILE", path)
    return path


@pytest.mark.parametrize("case", CASES, ids=lambda case: case["name"])
def test_shared_proxy_contract(case, isolated_config, monkeypatch):
    isolated_config.write_text(json.dumps({"proxy": case["defaults"]}))
    for key, value in {**case["env"], **case.get("overrides", {})}.items():
        monkeypatch.setenv(key, value)
    assert proxy_environment(case["env"]) == case["expected"]
    assert proxy_status() == {"source": case["source"]}


@pytest.mark.parametrize("provider", ["claude", "codex", "agy", "gemini", "grok", "muse", "local"])
def test_fake_cli_receives_default(provider, isolated_config, tmp_path, monkeypatch):
    isolated_config.write_text(json.dumps({"proxy": {"default": "http://proxy.example:8181", "no_proxy": "internal"}}))
    home, workspace = tmp_path / "home", tmp_path / "workspace"
    home.mkdir()
    workspace.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    for key in ("CODEX_HOME", "CLAUDE_CONFIG_DIR", "GEMINI_CONFIG_DIR", "AGY_CONFIG_DIR", "ANTIGRAVITY_CONFIG_DIR"):
        monkeypatch.delenv(key, raising=False)
    # Meet master's host-credit guard with fake settings; do not mock it out.
    for rel in (".gemini/settings.json", ".gemini/antigravity-cli/settings.json"):
        path = home / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('{"useG1Credits":false}')
    fake = tmp_path / "fake_cli.py"
    fake.write_text("import os,json; print(json.dumps({k:v for k,v in os.environ.items() if k.lower() in ('http_proxy','https_proxy','all_proxy','no_proxy')}))")
    with patch("makewand.sandbox.is_bwrap_available", return_value=False), patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"):
        with provider_sandbox_launch(provider, [sys.executable, str(fake)], str(workspace), readonly=True) as launch:
            result = subprocess.run(launch.cmd, env={**os.environ, **launch.env}, capture_output=True, text=True, check=True)
    got = json.loads(result.stdout)
    assert all(got[key] == "http://proxy.example:8181" for key in PROXY_KEYS)
    assert got["NO_PROXY"] == got["no_proxy"] == "internal"
    assert not any(key in os.environ for key in PROXY_KEYS)


def test_bwrap_default_and_offline(isolated_config, tmp_path):
    isolated_config.write_text('{"proxy":{"default":"http://proxy.example:8181","no_proxy":"internal"}}')
    with patch("makewand.sandbox.is_bwrap_available", return_value=True):
        online = wrap_bwrap(["echo", "fixture"], workspace=str(tmp_path), allow_network=True)
        offline = wrap_bwrap(["echo", "fixture"], workspace=str(tmp_path), allow_network=False)
    for key in PROXY_KEYS:
        assert online[online.index(key) + 1] == "http://proxy.example:8181"
        assert key not in offline


def test_gemini_explicit_bypass(isolated_config, monkeypatch):
    isolated_config.write_text('{"proxy":{"default":"http://proxy.example:8181","no_proxy":"internal"}}')
    before = proxy_environment({}, provider="agy")
    assert before["NO_PROXY"] == "internal"
    monkeypatch.setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "1")
    after = proxy_environment({}, provider="gemini")
    assert "googleapis.com" in after["NO_PROXY"]
    assert after["NO_PROXY"] == after["no_proxy"]
    monkeypatch.setenv("MAKEWAND_GEMINI_USE_PROXY", "1")
    assert proxy_environment({}, provider="gemini") == before


@pytest.mark.parametrize("target", ["https://api.example/v1", "http://local.example/v1", "https://generativelanguage.googleapis.com/v1"])
def test_api_opener_uses_defaults(target, isolated_config):
    isolated_config.write_text('{"proxy":{"default":"http://proxy.example:8181","no_proxy":"internal"}}')
    with patch("urllib.request.build_opener") as build:
        urlopen(target, timeout=1)
    handler = build.call_args.args[0]
    assert handler.proxies["http"] == handler.proxies["https"] == "http://proxy.example:8181"
    assert handler.proxies["no"] == "internal"
    build.return_value.open.assert_called_once_with(target, timeout=1)


def test_status_never_discloses_url(isolated_config, monkeypatch):
    isolated_config.write_text('{"proxy":{"default":"http://username:password@proxy.example:8181"}}')
    assert proxy_status() == {"source": "config_default"}
    monkeypatch.setenv("HTTP_PROXY", "malformed://username:password@")
    assert proxy_status() == {"source": "environment"}


@pytest.mark.parametrize("proxy", [[], "bad", {"default": 123}, {"no_proxy": False}])
def test_invalid_proxy_config_rejected(proxy, isolated_config):
    isolated_config.write_text(json.dumps({"proxy": proxy}))
    with pytest.raises(config.ConfigError):
        proxy_environment({})


@pytest.mark.parametrize("target,no_proxy,bypass", [
    ("http://127.0.0.1/v1", "", False),
    ("https://api.example/v1", "", False),
    ("https://host.internal", ".internal", True),
    ("https://internal", ".internal", True),
    ("http://10.1.2.3", "10.0.0.0/8", True),
    ("https://port.example:8443", "port.example:8443", True),
    ("https://port.example", "port.example:8443", False),
    ("https://api.example", "*", True),
])
def test_api_no_proxy(target, no_proxy, bypass):
    from makewand.proxy import _bypass
    from urllib.parse import urlparse
    assert _bypass(urlparse(target), no_proxy) is bypass


def test_api_routes_and_local_use_proxy(isolated_config):
    import threading
    from makewand.providers.api_client import _perform_request
    from makewand.providers.local import is_local_model_available
    import urllib.request
    isolated_config.write_text('{"proxy":{"default":"http://proxy.example:8181"}}')
    with patch("urllib.request.build_opener") as build:
        response = build.return_value.open.return_value.__enter__.return_value
        response.status = 200
        response.read.return_value = b'{"data":[{"id":"fixture-model"}]}'
        req = urllib.request.Request("https://api.example/v1", data=b'{}')
        assert _perform_request(req, 1, False, "", 0, threading.Event(), [], [])[0] == 200
        assert is_local_model_available()[0]
        assert build.call_count == 2
        for call in build.call_args_list:
            assert call.args[0].proxies["http"] == "http://proxy.example:8181"


def test_status_json_source_with_static_cache(isolated_config, capsys):
    from argparse import Namespace
    from makewand.cli import cmd_status
    isolated_config.write_text('{"proxy":{"default":"http://username:password@proxy.example:8181"}}')
    with patch("makewand.cli._status_cache", return_value={}), patch("makewand.config.get_provider_execution_mode", return_value="none"):
        cmd_status(Namespace(json=True, probe=False))
    out = capsys.readouterr().out
    assert json.loads(out)["proxy"] == {"source": "config_default"}
    assert "username" not in out and "password" not in out and "proxy.example" not in out


def test_gemini_bypass_merges_existing_cases(isolated_config, monkeypatch):
    monkeypatch.setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "1")
    original = {"NO_PROXY": "upper.example", "no_proxy": "lower.example"}
    got = proxy_environment(original, provider="gemini")
    assert got["NO_PROXY"] == got["no_proxy"]
    assert got["NO_PROXY"].startswith("upper.example,lower.example,")
    assert original == {"NO_PROXY": "upper.example", "no_proxy": "lower.example"}
    assert proxy_environment(got, provider="gemini") == got


@pytest.mark.parametrize("default", [None, "http://proxy.example:8181"])
def test_muse_guard_uses_configured_default_only(default, isolated_config, tmp_path):
    from contextlib import ExitStack
    from makewand.providers.muse import execute_muse_task
    isolated_config.write_text(json.dumps({"proxy": {"default": default}}))
    with ExitStack() as stack:
        for target, value in [
            ("makewand.health.load_status_cache", {}),
            ("makewand.config.has_subscription_configured", True),
            ("makewand.providers.muse.detect_muse_guard", (True, None)),
            ("makewand.providers.muse.verify_muse_guard", (True, "")),
            ("makewand.providers.muse.get_muse_executable", ("fixture-muse", [])),
            ("makewand.sandbox.is_bwrap_available", False),
            ("makewand.sandbox.authorize_unsandboxed_launch", "fixture"),
            ("makewand.sandbox._trusted_system_binary", None),
            ("makewand.discovery.get_provider_model_tier", {"model": "default", "effort": "none"}),
        ]:
            stack.enter_context(patch(target, return_value=value))
        run = stack.enter_context(patch("makewand.providers.muse.run_subprocess", return_value=(0, "fixture", "", None)))
        assert execute_muse_task("fixture", cwd=str(tmp_path), repo_root=str(tmp_path), readonly=True)[0]
    env = run.call_args.kwargs["extra_env"]
    if default:
        assert all(env[key] == default for key in PROXY_KEYS)
    else:
        assert not any(key in env for key in PROXY_KEYS)


def test_api_proxy_auth_and_bypass_are_request_local():
    import base64
    import urllib.request
    from makewand.proxy import _ProxyHandler
    handler = _ProxyHandler({"https": "http://user:p%40ss@proxy.example:8181", "no": "internal"})
    with patch("urllib.request.proxy_bypass", side_effect=AssertionError("global proxy bypass must not be consulted")):
        proxied = urllib.request.Request("https://api.example/v1")
        assert handler.proxy_open(proxied, handler.proxies["https"], "https") is None
        assert proxied.host == "proxy.example:8181"
        assert proxied.get_header("Proxy-authorization") == "Basic " + base64.b64encode(b"user:p@ss").decode("ascii")
        direct = urllib.request.Request("https://internal/v1")
        assert handler.proxy_open(direct, handler.proxies["https"], "https") is None
        assert direct.host == "internal"
        assert direct.get_header("Proxy-authorization") is None
