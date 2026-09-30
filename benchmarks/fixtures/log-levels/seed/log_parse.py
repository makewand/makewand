def parse_level(line):
    return line.split(']')[0].strip('[').lower()
