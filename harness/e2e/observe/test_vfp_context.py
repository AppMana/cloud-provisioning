import json
import pathlib
import unittest
import xml.etree.ElementTree as ET

import vfp_context as vfp

ROOT = pathlib.Path(__file__).parent / 'testdata'


class NativeVFPContextTest(unittest.TestCase):
    def fixture(self, version):
        return (ROOT / f'windows{version}-vfp-events.jsonl').read_text()

    def mutate(self, action):
        rows = []
        for line in self.fixture('2022').splitlines():
            root = ET.fromstring(json.loads(line)['xml'])
            action(root)
            rows.append(json.dumps({'xml': ET.tostring(root, encoding='unicode')}))
        return '\n'.join(rows)

    def test_observed_wireguard_and_api_ports_are_network_order(self):
        rows = vfp.flow_events(self.fixture('2022'))
        incoming = next(x for x in rows if x['eventID'] == 110)
        self.assertEqual((incoming['source'], incoming['sourcePort'], incoming['destination'],
                          incoming['destinationPort'], incoming['protocol']),
                         ('173.228.73.13', 51820, '172.29.0.24', 51820, 17))
        rows = vfp.flow_events(self.fixture('2025'))
        incoming = next(x for x in rows if x['eventID'] == 110)
        self.assertEqual((incoming['source'], incoming['sourcePort'], incoming['destination'],
                          incoming['destinationPort'], incoming['protocol']),
                         ('172.29.0.68', 50411, '10.10.0.14', 6443, 6))
        self.assertEqual(incoming['fields']['DstPort'], '11033')
        self.assertEqual(incoming['fields']['Status'], '0')

    def test_both_native_manifests_declare_network_field_types(self):
        ns = '{http://schemas.microsoft.com/win/2004/08/events}'
        for version in ['2022', '2025']:
            manifest = json.loads((ROOT / f'windows{version}-vfp-templates.json').read_text())
            self.assertEqual(manifest['provider'], vfp.PROVIDER)
            for event in manifest['events']:
                if event['Id'] not in vfp.FLOW_EVENTS:
                    continue
                fields = {f.get('name'): f.get('outType') for f in
                          ET.fromstring(event['Template']).findall(ns + 'data')}
                self.assertEqual(fields['SrcIpv4Addr'], 'win:IPv4')
                self.assertEqual(fields['DstPort'], 'win:Port')

    def test_native_vxlan_flow_context_retains_encapsulation(self):
        rows = vfp.flow_events((ROOT / 'windows2025-vfp-vxlan-flow.jsonl').read_text())
        self.assertEqual([r['eventID'] for r in rows], [600, 600, 608, 608])
        self.assertEqual({(r['source'], r['sourcePort'], r['destination'], r['destinationPort'])
                          for r in rows},
                         {('10.244.114.4', 52145, '10.244.42.6', 8081),
                          ('10.244.42.6', 8081, '10.244.114.4', 52145)})
        for row in rows:
            self.assertEqual(row['fields']['NumEncaps'], '1')
            self.assertEqual(row['fields']['EncapType0'], 'VXLAN')
            self.assertEqual(row['fields']['TenantId0'], '4096')
        # Flow creation/deletion supplies context, not proof of packet loss.
        self.assertTrue(all('drop' not in row for row in rows))

    def test_forwarding_fallback_is_not_a_flow_or_drop_claim(self):
        for version in ['2022', '2025']:
            rows = vfp.flow_events('\ufeff\n' + self.fixture(version) + '\n')
            self.assertEqual({r['eventID'] for r in rows}, vfp.FLOW_EVENTS)
            self.assertEqual(len(rows), 6)
            self.assertTrue(all('drop' not in r for r in rows))

    def test_unrecognized_provider_is_excluded(self):
        self.assertEqual(vfp.flow_events(self.fixture('2022').replace(vfp.PROVIDER, 'unknown')), [])

    def test_changed_schema_and_duplicate_fields_fail_closed(self):
        def version(root):
            root.find(vfp.NS + 'System/' + vfp.NS + 'Version').text = '99'

        def duplicate(root):
            data = root.find(vfp.NS + 'EventData')
            ET.SubElement(data, vfp.NS + 'Data', Name='SrcPort').text = '1'

        def no_system(root):
            root.remove(root.find(vfp.NS + 'System'))

        for action in [version, duplicate, no_system]:
            with self.subTest(action=action.__name__), self.assertRaises(ValueError):
                vfp.flow_events(self.mutate(action))

    def test_integer_overflow_is_rejected(self):
        for value, size in [('-1', 2), ('65536', 2), ('4294967296', 4)]:
            with self.assertRaises(ValueError):
                vfp.network_integer(value, size)

    def test_native_failed_and_successful_inbound_actions(self):
        fixture = json.loads((ROOT / 'windows-vfp-decap-cases.json').read_text())
        self.assertEqual(len(fixture['cases']), 9)
        for case in fixture['cases']:
            with self.subTest(version=case['windowsServer'], kind=case['kind'], index=case['dropIndex']):
                text = '\n'.join(json.dumps(e) for e in case['events'])
                args = [case[k] for k in ('source', 'sourcePort', 'destination', 'destinationPort', 'portName')]
                rows = vfp.inbound_vxlan_creations(text, *args)
                self.assertEqual(len(rows), 1)
                self.assertEqual(rows[0]['encapsulationAction'], case['expectedAction'])
                self.assertEqual(rows[0]['fields']['TenantId0'], '4096')
                self.assertEqual(vfp.inbound_vxlan_creations(text, *args[:-1], 'unrelated-port'), [])
                self.assertEqual(vfp.inbound_vxlan_creations(text, args[0], 1, *args[2:]), [])

    def test_native_encapsulation_selector_is_not_a_transport_port(self):
        fixture = json.loads((ROOT / 'windows-vfp-decap-cases.json').read_text())
        row, = vfp.flow_events(json.dumps(fixture['encapsulationSelectorEvent']))
        self.assertEqual(row['protocol'], 252)
        self.assertEqual(row['encapsulationSelectors'], dict(source=0, destination=4096))
        self.assertIsNone(row['sourcePort'])
        self.assertIsNone(row['destinationPort'])

    def test_unavailable_transposition_is_not_inferred(self):
        fixture = json.loads((ROOT / 'windows-vfp-decap-cases.json').read_text())
        case = fixture['cases'][0]
        root = ET.fromstring(case['events'][0]['xml'])
        for field in root.findall(vfp.NS + 'EventData/' + vfp.NS + 'Data'):
            if field.get('Name') == 'EncapTransposition0':
                field.text = None
        text = json.dumps(dict(xml=ET.tostring(root, encoding='unicode')))
        rows = vfp.inbound_vxlan_creations(text, *[case[k] for k in
            ('source', 'sourcePort', 'destination', 'destinationPort', 'portName')])
        self.assertIsNone(rows[0]['encapsulationAction'])


if __name__ == '__main__':
    unittest.main()
