import unittest
from quotes import aws_rows, cost, D


class QuotesTest(unittest.TestCase):
    def test_fractional_gpu_and_latest_os_specific_quote(self):
        def call(region, operation, **inputs):
            if operation == 'describe-instance-types':
                return {'InstanceTypes': [{'InstanceType': 'g6f.large',
                    'VCpuInfo': {'DefaultVCpus': 2}, 'MemoryInfo': {'SizeInMiB': 8192},
                    'GpuInfo': {'TotalGpuMemoryInMiB': 2861, 'Gpus': [
                        {'Count': 0, 'LogicalGpuCount': 1, 'GpuPartitionSize': 0.125}]}}]}
            return {'SpotPriceHistory': [dict(InstanceType='g6f.large', AvailabilityZone='r-a',
                ProductDescription=os, SpotPrice=price, Timestamp=timestamp)
                for os, price, timestamp in [('Windows', '.12', '2026-09-06T01:00:00Z'),
                    ('Windows', '.50', '2026-09-06T00:00:00Z'),
                    ('Linux/UNIX', '.03', '2026-09-06T01:00:00Z')]]}
        rows = aws_rows('r', ['g6f.large'], '2026-09-06T02:00:00Z', call)
        self.assertEqual(len(rows), 2)
        windows = next(r for r in rows if r['os'] == 'windows')
        self.assertEqual(windows['logicalGPUs'], 1)
        self.assertEqual(windows['hourlyUSD'], '.12')
        self.assertTrue(windows['windowsLicenseIncluded'])
        self.assertFalse(windows['capacityVerified'])

    def test_minimum_charge_changes_short_session_ranking(self):
        aws = dict(hourlyUSD='.12', minimumSeconds=60, billingIncrementSeconds=1)
        vultr = dict(hourlyUSD='.059', minimumSeconds=3600, billingIncrementSeconds=3600)
        self.assertLess(cost(aws, 1200), cost(vultr, 1200))
        self.assertEqual(cost(vultr, 3601), D('.118'))
        for bad in (0, -1, float('nan'), float('inf')):
            with self.assertRaises(ValueError):
                cost(aws, bad)


if __name__ == '__main__':
    unittest.main()
