import pathlib
import unittest

from udp_capture import exchange


class NativeUDPExchange(unittest.TestCase):
    def setUp(self):
        self.text = (pathlib.Path(__file__).parent/'testdata/windows2025-dual-failed-flows.tsv').read_text()

    def test_native_windows_request_is_observed_without_a_reply(self):
        result = exchange(self.text, '10.244.114.4', 59926, '10.244.42.6')
        self.assertEqual(result['request']['appearances'], 16)
        self.assertEqual(result['request']['ipIDs'], ['0x54a7'])
        self.assertEqual(result['request']['trafficClasses'], ['0x00'])
        self.assertEqual(result['reply']['appearances'], 0)

    def test_failed_client_can_have_both_request_and_reply_in_capture(self):
        result = exchange(self.text, '10.244.190.81', 41110, '10.244.42.6')
        self.assertEqual(result['request']['appearances'], 20)
        self.assertEqual(result['reply']['appearances'], 16)
        self.assertEqual(len(result['reply']['payloadSHA256']), 1)

    def test_other_port_cannot_borrow_observed_packets(self):
        result = exchange(self.text, '10.244.114.4', 59927, '10.244.42.6')
        self.assertEqual(result['request']['appearances'], 0)
        self.assertEqual(result['reply']['appearances'], 0)

    def test_incomplete_export_is_rejected(self):
        with self.assertRaises(ValueError):
            exchange('1\t0\t10.0.0.1', '10.0.0.1', 1, '10.0.0.2')


if __name__ == '__main__':
    unittest.main()
