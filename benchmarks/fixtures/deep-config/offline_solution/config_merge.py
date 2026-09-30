from copy import deepcopy

def merge_values(payload):
    def merge(base, override):
        result = dict(base)
        for key, value in override.items():
            result[key] = merge(base[key], value) if key in base and isinstance(base[key], dict) and isinstance(value, dict) else value
        return result
    return deepcopy(merge(payload["base"], payload["override"]))
