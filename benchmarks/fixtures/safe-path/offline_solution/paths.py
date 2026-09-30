import re

def normalize_relative_path(value):
    path = value.replace("\\", "/")
    if path.startswith("/") or re.match(r"^[A-Za-z]:", path) or "\0" in path:
        raise ValueError("absolute or invalid path")
    parts = []
    for part in path.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                raise ValueError("path escapes root")
            parts.pop()
        else:
            parts.append(part)
    return "/".join(parts) or "."
