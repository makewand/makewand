def merge_values(payload):
    payload['base'].update(payload['override'])
    return payload['base']
