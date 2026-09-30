def recover_sequence(events):
    if any(event not in ("ok", "transient", "fatal") for event in events):
        raise ValueError("unknown event")
    attempts, delays = 0, []
    for index, event in enumerate(events[:3]):
        attempts += 1
        if event == "ok":
            return {"status": "ok", "attempts": attempts, "delays": delays}
        if event == "fatal":
            break
        if attempts < 3 and index + 1 < len(events):
            delays.append(2 ** (attempts - 1))
    return {"status": "failed", "attempts": attempts, "delays": delays}
