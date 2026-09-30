import re
from urllib.parse import parse_qsl

def parse_query(query):
    if re.search(r"%(?![0-9a-fA-F]{2})", query):
        raise ValueError("invalid escape")
    result = {}
    for key, value in parse_qsl(query, keep_blank_values=True, encoding="utf-8", errors="strict"):
        result.setdefault(key, []).append(value)
    return result
