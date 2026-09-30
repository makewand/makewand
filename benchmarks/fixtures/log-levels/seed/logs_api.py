from log_parse import parse_level

def summarize_logs(lines):
    return {'error': sum('ERROR' in line for line in lines)}
