"""Shared proxy defaults for CLI launches and API requests; never mutate os.environ."""

import os
import base64
import ipaddress
import urllib.request
from urllib.parse import unquote, urlparse

PROXY_KEYS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")
GOOGLE_HOSTS = ("googleapis.com", "google.com", "cloudcode-pa.googleapis.com")


def _defaults():
    from makewand.config import load_user_config
    defaults = load_user_config().get("proxy") or {}
    return (os.environ.get("MAKEWAND_DEFAULT_PROXY", defaults.get("default") or ""),
            os.environ.get("MAKEWAND_DEFAULT_NO_PROXY", defaults.get("no_proxy") or ""))


def proxy_environment(env=None, *, provider=None):
    """Copy env, fill absent proxy sets, then apply explicit Gemini bypass.

    A present empty variable suppresses defaults. Existing NO_PROXY variables
    remain untouched when injecting a default, even if only one case exists.
    """
    result = dict(os.environ if env is None else env)
    default, no_proxy = _defaults()
    if not any(key in result for key in PROXY_KEYS) and default:
        result.update({key: default for key in PROXY_KEYS})
        if no_proxy and "NO_PROXY" not in result and "no_proxy" not in result:
            result.update(NO_PROXY=no_proxy, no_proxy=no_proxy)
    if provider in ("agy", "gemini", "google") and _flag("MAKEWAND_GEMINI_BYPASS_PROXY") and not _flag("MAKEWAND_GEMINI_USE_PROXY"):
        hosts = []
        for value in (result.get("NO_PROXY", ""), result.get("no_proxy", ""), ",".join(GOOGLE_HOSTS)):
            for host in value.split(","):
                host = host.strip()
                if host and host not in hosts:
                    hosts.append(host)
        result.update(NO_PROXY=",".join(hosts), no_proxy=",".join(hosts))
    return result


def _flag(key):
    return os.environ.get(key, "").strip().lower() in ("1", "true", "yes", "on")


def proxy_status():
    """Report source only, so even malformed proxy URLs cannot leak credentials."""
    if any(key in os.environ for key in PROXY_KEYS):
        return {"source": "environment"}
    default, _ = _defaults()
    if not default:
        return {"source": "none"}
    return {"source": "environment" if "MAKEWAND_DEFAULT_PROXY" in os.environ else "config_default"}


def _bypass(target, no_proxy):
    host = (target.hostname or "").lower()
    port = target.port or (443 if target.scheme == "https" else 80)
    for item in no_proxy.split(","):
        item = item.strip().lower()
        if not item:
            continue
        if item == "*":
            return True
        try:
            if "/" in item and ipaddress.ip_address(host) in ipaddress.ip_network(item, strict=False):
                return True
        except ValueError:
            pass
        parsed = urlparse("//" + item)
        try:
            if parsed.port is not None:
                if parsed.port != port:
                    continue
                item = parsed.hostname or ""
        except ValueError:
            pass
        item = item.removeprefix("*.")
        domain = item.lstrip(".")
        if host == domain or host.endswith("." + domain):
            return True
    return False


class _ProxyHandler(urllib.request.ProxyHandler):
    def proxy_open(self, req, proxy, type):
        if _bypass(urlparse(req.full_url), self.proxies.get("no", "")):
            return None
        # ProxyHandler's implementation consults the global environment (and
        # OS proxy settings on macOS). Use only this request's resolved policy.
        original_type = req.type
        parsed = urlparse(proxy if "://" in proxy else "http://" + proxy)
        if not parsed.hostname:
            raise ValueError("invalid proxy URL")
        if parsed.username is not None and parsed.password is not None:
            credentials = f"{unquote(parsed.username)}:{unquote(parsed.password)}"
            encoded = base64.b64encode(credentials.encode()).decode("ascii")
            req.add_header("Proxy-authorization", "Basic " + encoded)
        req.set_proxy(unquote(parsed.netloc.rsplit("@", 1)[-1]), parsed.scheme)
        if original_type == parsed.scheme or original_type == "https":
            return None
        return self.parent.open(req, timeout=req.timeout)


def urlopen(req, *args, **kwargs):
    """Use a request-local opener for defaults, ALL_PROXY and explicit bypass."""
    host = urlparse(req.full_url if hasattr(req, "full_url") else req).hostname or ""
    google = any(host == domain or host.endswith("." + domain) for domain in GOOGLE_HOSTS)
    effective = proxy_environment(provider="gemini" if google else None)
    if effective == dict(os.environ) and not any(os.environ.get(key) for key in PROXY_KEYS):
        return urllib.request.urlopen(req, *args, **kwargs)
    proxies = {}
    for name in ("http", "https", "all", "no"):
        upper, lower = name.upper() + "_PROXY", name + "_proxy"
        value = effective.get(lower, effective.get(upper, ""))
        if value:
            proxies[name] = value
    if proxies.get("all"):
        for name in ("http", "https"):
            proxies.setdefault(name, proxies["all"])
    return urllib.request.build_opener(_ProxyHandler(proxies)).open(req, *args, **kwargs)
