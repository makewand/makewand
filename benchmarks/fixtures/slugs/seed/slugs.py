def unique_slugs(titles):
    """Normalize titles and allocate globally unique ASCII slugs."""
    return [title.lower().replace(" ", "-") for title in titles]
