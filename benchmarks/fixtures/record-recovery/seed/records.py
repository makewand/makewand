def recover_records(rows):
    return {'records': [{'id': row['id'], 'value': int(row['value'])} for row in rows], 'errors': []}
