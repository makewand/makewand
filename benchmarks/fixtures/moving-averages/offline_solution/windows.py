def moving_averages(payload):
    values, width = payload["values"], payload["width"]
    if isinstance(width, bool) or not isinstance(width, int) or width <= 0:
        raise ValueError("width must be a positive integer")
    return [sum(values[index:index + width]) / width for index in range(len(values) - width + 1)]
