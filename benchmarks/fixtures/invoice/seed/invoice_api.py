from pricing import price_line

def total_invoice(lines):
    return str(sum(float(price_line(line)) for line in lines))
