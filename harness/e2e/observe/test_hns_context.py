import pathlib
import datetime
import json
import hashlib
import unittest
import xml.etree.ElementTree as ET

import hns_context as hns
import vfp_context

ROOT = pathlib.Path(__file__).parent / 'testdata'
PORTS = {'2022': '7D2CED1C-ED91-49F6-9FA0-0A2184B37676',
         '2025': '1903CA54-D200-479E-B7F3-A3EC18AE1313'}


class NativeHNSContextTest(unittest.TestCase):
    def fixture(self, version='2022'):
        return (ROOT / f'windows{version}-hns-layers.xml').read_text()

    def pair(self):
        root = ET.fromstring(self.fixture())
        matches = []
        for e in root:
            fields = {d.get('Name'): d.text for d in e.findall(hns.NS+'EventData/'+hns.NS+'Data')}
            if fields.get('layerId') == 'VNET_ENCAP_LAYER' and fields.get('portId') == PORTS['2022']:
                matches.append(e)
        return matches[:2]

    def xml(self, events):
        root = ET.Element('Events')
        root.extend(events)
        return ET.tostring(root, encoding='unicode')

    def test_native_intervals_on_both_versions(self):
        for version, count, duration in [('2022', 9, 32290), ('2025', 5, 15316)]:
            rows = hns.layer_rebuilds(self.fixture(version), PORTS[version])
            self.assertEqual(len(rows), count)
            self.assertTrue(all(r['complete'] for r in rows))
            self.assertEqual(rows[0]['durationMicroseconds'], duration)

    def test_reused_activity_pairs_each_successor_not_last_addition(self):
        rows = hns.layer_rebuilds(self.fixture(), PORTS['2022'])
        self.assertEqual(rows[1]['activity'], rows[2]['activity'])
        self.assertEqual([r['durationMicroseconds'] for r in rows[1:4]], [1227, 335, 1101])

    def test_unrelated_ports_layers_and_provider_errors_do_not_match(self):
        self.assertEqual(hns.layer_rebuilds(self.fixture(), 'another-port'), [])
        self.assertEqual(hns.layer_rebuilds(self.fixture(), PORTS['2022'], 'another-layer'), [])
        self.assertEqual(hns.layer_rebuilds(self.fixture().replace(hns.PROVIDER, 'unrelated'), PORTS['2022']), [])

    def test_capture_boundaries_are_not_complete_rebuilds(self):
        remove, add = self.pair()
        rows = hns.layer_rebuilds(self.xml([remove]), PORTS['2022'])
        self.assertFalse(rows[0]['complete'])
        self.assertIsNone(rows[0]['addedAt'])
        self.assertIsNone(rows[0]['durationMicroseconds'])
        self.assertEqual(hns.layer_rebuilds(self.xml([add]), PORTS['2022']), [])

    def test_repeated_removal_and_reversed_time_are_rejected(self):
        remove, add = self.pair()
        with self.assertRaisesRegex(ValueError, 'Repeated'):
            hns.layer_rebuilds(self.xml([remove, remove, add]), PORTS['2022'])
        with self.assertRaisesRegex(ValueError, 'event-time order'):
            hns.layer_rebuilds(self.xml([add, remove]), PORTS['2022'])

    def test_undecoded_hns_event_is_not_an_empty_result(self):
        with self.assertRaisesRegex(ValueError, 'Undecoded HNS'):
            hns.layer_rebuilds((ROOT / 'windows2022-hns-undecoded.xml').read_text(), PORTS['2022'])

    def test_missing_native_context_is_rejected(self):
        for kind in ['system', 'activity', 'port', 'timezone']:
            remove, _ = self.pair()
            system = remove.find(hns.NS+'System')
            if kind == 'system': remove.remove(system)
            elif kind == 'activity': system.remove(system.find(hns.NS+'Correlation'))
            elif kind == 'port':
                field = next(d for d in remove.findall(hns.NS+'EventData/'+hns.NS+'Data') if d.get('Name') == 'portId')
                field.text = ''
            else: system.find(hns.NS+'TimeCreated').set('SystemTime', '2026-09-09T03:15:50')
            with self.assertRaises(ValueError): hns.layer_rebuilds(self.xml([remove]), PORTS['2022'])

    def test_duplicate_fields_are_rejected(self):
        remove, _ = self.pair()
        ET.SubElement(remove.find(hns.NS+'EventData'), hns.NS+'Data', Name='portId').text = PORTS['2022']
        with self.assertRaisesRegex(ValueError, 'duplicate'):
            hns.layer_rebuilds(self.xml([remove]), PORTS['2022'])

    def test_native_failed_flows_and_drops_fall_inside_layer_rebuilds(self):
        def when(value):
            parsed = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
            return parsed if parsed.tzinfo else parsed.replace(tzinfo=datetime.timezone.utc)
        cases = json.loads((ROOT / 'windows-hns-decap-cases.json').read_text())['cases']
        self.assertEqual(len(cases), 6)
        for case in cases:
            spans = hns.layer_rebuilds(self.fixture(case['windowsServer']), case['port'])
            drop = when(case['dropTimestamp'])
            matched = [s for s in spans if s['complete'] and when(s['removedAt']) <= drop <= when(s['addedAt'])]
            self.assertEqual(len(matched), 1)
            flows = vfp_context.inbound_vxlan_creations(json.dumps({'xml': case['nativeFlowXML']}),
                *case['tuple'], case['port'])
            self.assertEqual(len(flows), 1)
            self.assertEqual(flows[0]['encapsulationAction'], 'Ignore')
            self.assertLessEqual(when(matched[0]['removedAt']), when(flows[0]['timestamp']))
            self.assertLessEqual(when(flows[0]['timestamp']), drop)
            self.assertIn(case['dropTimestamp'], case['dropHeader'])


class NativeDeferredSpaceTest(unittest.TestCase):
    network = '0782A040-B640-4E75-A4DE-20B2B1F38A70'

    def fixture(self):
        return (ROOT / 'windows2022-hns-deferred-spaces.xml').read_text()

    def test_native_cleanup_follows_api_deletion_and_overlaps_failed_reply(self):
        evidence = json.loads((ROOT / 'windows-hns-deferred-spaces.json').read_text())
        self.assertEqual(hashlib.sha256(self.fixture().encode()).hexdigest(), evidence['fixtureSHA256'])
        self.assertEqual(len(ET.fromstring(self.fixture())), evidence['nativeEventElements'])
        rows = hns.network_space_removals(self.fixture(), PORTS['2022'], self.network)
        self.assertEqual([r['family'] for r in rows], ['ipv6', 'ipv4'])
        api = datetime.datetime.fromisoformat(evidence['apiRemoval']['removedAt'].replace('Z', '+00:00'))
        drop = datetime.datetime.fromisoformat(evidence['dropAt'])
        for row in rows:
            removed = datetime.datetime.fromisoformat(row['removedAt'])
            self.assertGreater((removed - api).total_seconds(), 30)
            self.assertGreater(removed, drop)
            self.assertLess((removed - drop).total_seconds(), .001)
            self.assertNotIn('complete', row)

    def test_exact_network_and_port_are_required(self):
        self.assertEqual(hns.network_space_removals(self.fixture(), 'other-port', self.network), [])
        self.assertEqual(hns.network_space_removals(self.fixture(), PORTS['2022'], 'other-network'), [])
        rows = hns.network_space_removals(self.fixture(), PORTS['2022'].lower(), self.network.lower())
        self.assertEqual(len(rows), 2)

    def test_undecoded_trace_cannot_claim_no_pending_cleanup(self):
        with self.assertRaisesRegex(ValueError, 'Undecoded HNS'):
            hns.network_space_removals((ROOT / 'windows2022-hns-undecoded.xml').read_text(),
                                      PORTS['2022'], self.network)

    def test_invalid_space_context_is_rejected(self):
        for mutation in ('port', 'space', 'duplicate', 'timezone', 'reversed'):
            root = ET.fromstring(self.fixture())
            removals = [e for e in root if e.findtext(hns.NS+'RenderingInfo/'+hns.NS+'Task') == 'RemovingSpace']
            event = removals[0]
            if mutation in ('port', 'space'):
                key = 'portId' if mutation == 'port' else 'spaceId'
                next(f for f in event.findall(hns.NS+'EventData/'+hns.NS+'Data') if f.get('Name') == key).text = ''
            elif mutation == 'duplicate':
                ET.SubElement(event.find(hns.NS+'EventData'), hns.NS+'Data', Name='spaceId').text = self.network
            elif mutation == 'timezone':
                event.find(hns.NS+'System/'+hns.NS+'TimeCreated').set('SystemTime', '2026-09-09T04:19:00')
            else:
                root = ET.Element('Events')
                root.extend(reversed(removals))
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                hns.network_space_removals(ET.tostring(root, encoding='unicode'), PORTS['2022'], self.network)
