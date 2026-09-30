def lower_bound(payload):
    items, target = payload["items"], payload["target"]
    if any(left > right for left, right in zip(items, items[1:])):
        raise ValueError("items must be sorted")
    low, high = 0, len(items)
    while low < high:
        middle = (low + high) // 2
        if items[middle] < target:
            low = middle + 1
        else:
            high = middle
    return low
