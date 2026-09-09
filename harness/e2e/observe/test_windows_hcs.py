import copy
import json
import pathlib
import unittest
from windows_hcs import timeline


class TimelineTest(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((pathlib.Path(__file__).parent / 'testdata/windows-hcs-start-error.json').read_text())

    def test_native_start_then_rpc_failure(self):
        f = self.fixture
        result = timeline(f['pod'], f['events'], 'render-nvenc')
        self.assertEqual(result['terminalReason'], 'StartError')
        stages = result['stages']
        self.assertEqual([e['stage'] for e in stages], ['createSystem', 'startSystem', 'containerStarted', 'createProcess', 'terminateSystem', 'terminateSystem'])
        self.assertTrue(stages[0]['operationPending'])
        self.assertEqual(stages[3]['result'], '0x800706ba')
        self.assertFalse(stages[3]['operationPending'])
        self.assertNotIn('parameters', json.dumps(result))

    def test_unrelated_container_cannot_supply_start(self):
        f = self.fixture
        events = copy.deepcopy(f['events'])
        for e in events:
            e['message'] = e['message'].replace('[' , '[other-', 1)
        self.assertEqual(timeline(f['pod'], events, 'render-nvenc')['stages'], [])

    def test_timezone_offsets_are_normalized_and_naive_time_rejected(self):
        f = self.fixture
        event = next(e for e in f['events'] if e['id'] == 2022)
        event['at'] = '2026-09-07T16:05:52.810365-07:00'
        result = timeline(f['pod'], f['events'], 'render-nvenc')
        self.assertEqual(result['stages'][2]['stage'], 'containerStarted')
        event['at'] = '2026-09-07T23:05:52'
        with self.assertRaises(ValueError):
            timeline(f['pod'], f['events'], 'render-nvenc')

    def test_missing_or_ambiguous_identity_rejected(self):
        f = self.fixture
        with self.assertRaises(ValueError):
            timeline(f['pod'], f['events'], 'other')
        f['pod']['status']['containerStatuses'] *= 2
        with self.assertRaises(ValueError):
            timeline(f['pod'], f['events'], 'render-nvenc')


if __name__ == '__main__':
    unittest.main()
