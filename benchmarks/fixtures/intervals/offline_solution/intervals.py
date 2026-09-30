def merge_intervals(intervals):
    result = []
    for start, end in sorted(intervals):
        if start > end:
            raise ValueError("reversed")
        if result and start <= result[-1][1]:
            result[-1] = (result[-1][0], max(end, result[-1][1]))
        else:
            result.append((start, end))
    return result
