def parse_query(query):
    return dict(field.split('=', 1) for field in query.split('&'))
