import copy
import json
import pathlib
import unittest
from windows_peers import evaluate

class NativePeerObservations(unittest.TestCase):
    def setUp(self):
        self.fixture=json.loads((pathlib.Path(__file__).parent/'testdata/windows-peer-transitions.json').read_text())

    def test_native_removal_preserves_survivor_counters(self):
        r=evaluate(self.fixture['removal'],require_removal=True)
        self.assertTrue(r['passed'])
        self.assertEqual(r['survivorCount'],9)
        self.assertEqual(len(r['membershipChanges'][0]['removed']),1)

    def test_native_old_backend_counter_resets_fail(self):
        r=evaluate(self.fixture['oldReset'])
        self.assertFalse(r['passed'])
        self.assertTrue(r['counterDecreases'])

    def test_native_capture_started_after_addition_cannot_qualify_addition(self):
        r=evaluate(self.fixture['missedAddition'],require_addition=True)
        self.assertTrue(r['checks']['noObservedCounterDecreases'])
        self.assertFalse(r['checks']['additionObserved'])
        self.assertFalse(r['passed'])

    def test_simulated_addition_does_not_imply_removal(self):
        # Reverse native membership sets, preserving chronological timestamps.
        rows=copy.deepcopy(self.fixture['removal'])
        rows[0]['peers'],rows[1]['peers']=rows[1]['peers'],rows[0]['peers']
        for p in rows[1]['peers']:
            p['txBytes']+=1000000;p['rxBytes']+=1000000
        r=evaluate(rows,require_addition=True,require_removal=True)
        self.assertTrue(r['checks']['additionObserved'])
        self.assertFalse(r['checks']['removalObserved'])

    def test_unrelated_membership_change_cannot_qualify_target(self):
        rows=self.fixture['removal']
        removed=(set(p['publicKeySHA256'] for p in rows[0]['peers'])-
                 set(p['publicKeySHA256'] for p in rows[1]['peers'])).pop()
        self.assertTrue(evaluate(rows,require_removal=True,expected_peer=removed)['passed'])
        self.assertFalse(evaluate(rows,require_removal=True,expected_peer='0'*64)['passed'])

    def test_invalid_capture_cannot_pass(self):
        for fault in ['duplicate','order','adapter','counter']:
            rows=copy.deepcopy(self.fixture['removal'])
            if fault=='duplicate':rows[0]['peers'].append(rows[0]['peers'][0])
            elif fault=='order':rows.reverse()
            elif fault=='adapter':rows[1]['interface']='another'
            else:rows[0]['peers'][0]['txBytes']=-1
            with self.subTest(fault=fault),self.assertRaises(ValueError):evaluate(rows)

    def test_adapter_down_fails_even_without_counter_reset(self):
        rows=copy.deepcopy(self.fixture['removal']);rows[1]['up']=False
        self.assertFalse(evaluate(rows)['passed'])
