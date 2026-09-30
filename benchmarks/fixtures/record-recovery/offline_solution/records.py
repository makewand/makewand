import re

def recover_records(rows):
    records, errors, seen = [], [], set()
    for index, row in enumerate(rows):
        try:
            if not isinstance(row, dict) or not isinstance(row.get("id"), str) or not row["id"].strip():
                raise ValueError("invalid id")
            identifier, value = row["id"].strip(), row.get("value")
            if isinstance(value, bool):
                raise ValueError("boolean value")
            if isinstance(value, str):
                if not re.fullmatch(r"[+-]?[0-9]+", value.strip()):
                    raise ValueError("invalid integer")
                value = int(value.strip())
            elif not isinstance(value, int):
                raise ValueError("invalid integer")
        except ValueError:
            errors.append({"index": index, "reason": "invalid"})
            continue
        if identifier in seen:
            errors.append({"index": index, "reason": "duplicate"})
            continue
        seen.add(identifier)
        records.append({"id": identifier, "value": value})
    return {"records": records, "errors": errors}
