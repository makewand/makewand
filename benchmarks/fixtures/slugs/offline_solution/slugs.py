import re
import unicodedata

def unique_slugs(titles):
    result, used = [], set()
    for title in titles:
        normalized = unicodedata.normalize("NFKD", title).encode("ascii", "ignore").decode().lower()
        base = re.sub("[^a-z0-9]+", "-", normalized).strip("-") or "item"
        slug, suffix = base, 2
        while slug in used:
            slug = f"{base}-{suffix}"
            suffix += 1
        result.append(slug)
        used.add(slug)
    return result
