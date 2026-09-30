import re

def parse_level(line):
    if not isinstance(line, str):
        return None
    match = re.fullmatch(r"\s*\[(debug|info|warning|error)\]\s+\S(?:.*\S)?\s*", line, re.IGNORECASE)
    return match.group(1).lower() if match else None
