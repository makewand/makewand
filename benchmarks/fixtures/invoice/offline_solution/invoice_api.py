from decimal import Decimal
from pricing import price_line

def total_invoice(lines):
    return format(sum((Decimal(price_line(line)) for line in lines), Decimal(0)), ".2f")
