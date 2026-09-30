from log_parse import parse_level

def summarize_logs(lines):
    counts = dict.fromkeys(("debug", "info", "warning", "error"), 0)
    for line in lines:
        level = parse_level(line)
        if level is not None:
            counts[level] += 1
    return counts
