from decimal import Decimal, InvalidOperation, ROUND_HALF_UP

def price_line(line):
    try:
        quantity, raw = line["quantity"], line["unit_price"]
        if isinstance(quantity, bool) or not isinstance(quantity, int) or quantity < 0 or not isinstance(raw, str):
            raise ValueError("invalid quantity or price")
        price = Decimal(raw)
        if not price.is_finite() or price < 0:
            raise ValueError("invalid price")
        return format((quantity * price).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP), ".2f")
    except (KeyError, TypeError, InvalidOperation) as error:
        raise ValueError("invalid line") from error
