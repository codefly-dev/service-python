import unittest


class CalculationTests(unittest.TestCase):
    def test_calculation(self):
        self.assertEqual(sum([1, 2, 3]), 6)
