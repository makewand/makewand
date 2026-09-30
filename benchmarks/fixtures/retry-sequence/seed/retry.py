def recover_sequence(events):
    return {'status': 'ok' if 'ok' in events else 'failed', 'attempts': len(events), 'delays': []}
