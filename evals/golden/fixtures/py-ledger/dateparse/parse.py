"""Date parsing helpers. Standard library only."""

from datetime import date


def parse_iso_date(value: str) -> date:
    """Parse a YYYY-MM-DD string into a date."""
    year, month, day = value.split("-")
    return date(int(year), int(month), int(day))


def days_between(start: str, end: str) -> int:
    """Return the number of days from start to end, both ISO dates."""
    return (parse_iso_date(end) - parse_iso_date(start)).days
