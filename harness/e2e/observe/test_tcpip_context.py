import json
import pathlib
import unittest

import pktmon
import tcpip_context

ROOT = pathlib.Path(__file__).parent / 'testdata'


class NativeTCPIPContextTest(unittest.TestCase):
    def replace_xml(self, text, old, new):
        rows = [json.loads(line) for line in text.splitlines() if line.strip()]
        for row in rows:
            row['xml'] = row['xml'].replace(old, new)
        return '\n'.join(json.dumps(row) for row in rows)

    def fixture(self, version):
        text = (ROOT / f'windows{version}-tcpip-events.jsonl').read_text(encoding='utf-8-sig')
        snapshot = json.loads((ROOT / f'windows{version}-tcpip-interfaces.json').read_text(encoding='utf-8-sig'))
        return text, snapshot

    def test_native_request_and_reply_drops_on_both_versions(self):
        for version, client, server, request_port, reply_port, outer_source, outer_dest, index in [
            ('2022', '10.244.42.6', '10.244.114.4', 52206, 65348, '172.29.0.68', '172.29.0.24', 18),
            ('2025', '10.244.114.4', '10.244.42.6', 49550, 50030, '172.29.0.24', '172.29.0.68', 17),
        ]:
            text, snapshot = self.fixture(version)
            packets = (ROOT / f'windows{version}-tcpip-request-reply-drops.txt').read_text()
            for port, destination_port in [(request_port, 8081), (8081, reply_port)]:
                with self.subTest(version=version, port=port):
                    drops = pktmon.correlate(packets, client, port, server, destination_port)['drops']
                    self.assertEqual(len(drops), 1)
                    result = tcpip_context.nearby(text, drops[0]['timestamp'], outer_source, outer_dest, snapshot)
                    self.assertTrue(result['uniqueCandidate'])
                    event = result['candidates'][0]
                    self.assertEqual(event['interfaceIndex'], index)
                    self.assertEqual(event['interfaces'][0]['CompartmentId'], 2)
                    self.assertTrue(event['addresses'][0]['IPAddress'].startswith('169.254.'))
                    self.assertEqual((event['reason'], event['transportProtocol']), (2, 0))
                    self.assertEqual(drops[0]['reason'], 'Not locally destined')

    def test_wrong_direction_time_or_inner_addresses_do_not_match(self):
        text, snapshot = self.fixture('2022')
        for timestamp, source, destination in [
            ('2026-09-08 22:26:47.763100700', '172.29.0.24', '172.29.0.68'),
            ('2026-09-08 22:26:47.763100700', '10.244.42.6', '10.244.114.4'),
            ('2026-09-08 22:27:47.763100700', '172.29.0.68', '172.29.0.24'),
        ]:
            self.assertEqual(tcpip_context.nearby(text, timestamp, source, destination, snapshot)['candidates'], [])

    def test_broad_window_preserves_two_candidates(self):
        text, snapshot = self.fixture('2022')
        result = tcpip_context.nearby(text, '2026-09-08 22:26:47.763171600',
            '172.29.0.68', '172.29.0.24', snapshot, window_microseconds=100)
        self.assertFalse(result['uniqueCandidate'])
        self.assertEqual(len(result['candidates']), 2)

    def test_missing_snapshot_mapping_is_not_inferred(self):
        text, _ = self.fixture('2025')
        result = tcpip_context.nearby(text, '2026-09-08 22:26:22.857088200',
            '172.29.0.24', '172.29.0.68', dict(interfaces=[], addresses=[]))
        self.assertTrue(result['uniqueCandidate'])
        self.assertEqual(result['candidates'][0]['interfaces'], [])

    def test_unrecognized_provider_and_event_are_excluded(self):
        text, _ = self.fixture('2022')
        self.assertEqual(tcpip_context.events(text.replace(tcpip_context.PROVIDER, 'other-provider')), [])
        self.assertEqual(tcpip_context.events(self.replace_xml(text, '<EventID>1215</EventID>', '<EventID>1216</EventID>')), [])

    def test_invalid_schema_or_sockaddr_is_rejected(self):
        text, _ = self.fixture('2022')
        for mutated in [self.replace_xml(text, '<Version>1</Version>', '<Version>99</Version>'),
                        self.replace_xml(text, "<Data Name='IfIndex'>", "<Data Name='Reason'>"),
                        self.replace_xml(self.replace_xml(text, '<System>', '<Other>'), '</System>', '</Other>')]:
            with self.assertRaises(ValueError):
                tcpip_context.events(mutated)
        for address in ['0200', '17000000AC1D00440000000000000000']:
            with self.assertRaises(ValueError):
                tcpip_context.ipv4_sockaddr(address)
        with self.assertRaises(ValueError):
            tcpip_context.nearby('', '', '', '', {}, 0)


if __name__ == '__main__':
    unittest.main()
