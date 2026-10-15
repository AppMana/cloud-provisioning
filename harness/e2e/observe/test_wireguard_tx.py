import unittest
from unittest.mock import patch

from wireguard_tx import program


class WireGuardTransmitProgram(unittest.TestCase):
    def test_exact_interface_and_network_byte_order(self):
        result = program('eth-test.1', ['192.0.2.1', '198.51.100.2'], 30)
        self.assertIn('str(args->name) == "eth-test.1"', result)
        self.assertIn('$dst == 0x010200c0 || $dst == 0x026433c6', result)
        self.assertIn('interval:s:30 { exit(); }', result)
        self.assertNotIn('@@', result)

    def test_unbounded_or_injectable_configuration_is_rejected(self):
        for interface in ['', 'x' * 16, 'eth0"/ { exit(); }', 'eth0\n']:
            with self.subTest(interface=interface), self.assertRaises(ValueError):
                program(interface, ['192.0.2.1'], 10)
        for seconds in [0, -1, 901, True, 1.5]:
            with self.subTest(seconds=seconds), self.assertRaises(ValueError):
                program('eth0', ['192.0.2.1'], seconds)
        for peers in [[], ['192.0.2.1'] * 2, ['192.0.2.0/24'], ['::1'],
                      ['0.0.0.0'], ['224.0.0.1'], ['255.255.255.255'], [1], ['192.0.2.1;exit()'],
                      [f'192.0.2.{i}' for i in range(1, 18)]]:
            with self.subTest(peers=peers), self.assertRaises(ValueError):
                program('eth0', peers, 10)

    def test_unsupported_host_byte_order_is_rejected(self):
        with patch('wireguard_tx.sys.byteorder', 'big'), self.assertRaises(ValueError):
            program('eth0', ['192.0.2.1'], 10)
