import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("host_check", Path(__file__).with_name("host-check.py"))
host_check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host_check)


class AdmissionTest(unittest.TestCase):
    def test_new_fleet_requires_memory_in_addition_to_reserve(self):
        self.assertTrue(host_check.evaluate(32, 200, dict(memory=0, io=0, cpu=0), 32, 8, 100))

    def test_healthy_existing_fleet(self):
        self.assertEqual(host_check.evaluate(8, 100, dict(memory=0, io=0, cpu=0), 0, 8, 100), [])

    def test_each_pressure_source_blocks_independently(self):
        for source in ("memory", "io", "cpu"):
            with self.subTest(source=source):
                pressure = dict(memory=0, io=0, cpu=0)
                pressure[source] = 25
                self.assertEqual(len(host_check.evaluate(64, 200, pressure, 0, 8, 100)), 1)

    def test_low_disk_blocks_even_with_free_ram(self):
        self.assertTrue(host_check.evaluate(64, 99, dict(memory=0, io=0, cpu=0), 0, 8, 100))


if __name__ == "__main__":
    unittest.main()
