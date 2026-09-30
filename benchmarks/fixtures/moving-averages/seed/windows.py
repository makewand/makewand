def moving_averages(payload):
    values, width = payload['values'], payload['width']
    return [sum(values[index:index + width]) / width for index in range(len(values))]
