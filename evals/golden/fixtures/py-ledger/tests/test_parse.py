import unittest
from datetime import date

from dateparse import days_between, parse_iso_date


class TestParse(unittest.TestCase):
    def test_parse_iso_date(self):
        self.assertEqual(parse_iso_date("2026-09-15"), date(2026, 9, 15))

    def test_days_between(self):
        self.assertEqual(days_between("2026-09-01", "2026-09-15"), 14)


if __name__ == "__main__":
    unittest.main()
