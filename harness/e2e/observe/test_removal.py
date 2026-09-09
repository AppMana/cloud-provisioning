import copy
import json
import pathlib
import unittest

from removal import evaluate, preflight


class NativeRemovalObservation(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-removal-pending.json').read_text())

    def observe(self, fixture=None):
        f = fixture or self.fixture
        return evaluate(f['before'], f['after'], f['receipt'])

    def baseline(self):
        f = json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-removal-complete.json').read_text())
        claim = f['receipt']['claim']
        machine = next(m for m in f['before']['machines']['items'] if m['metadata']['name'] == claim)
        return f['before'], claim, machine['metadata']['uid']

    def test_native_ready_baseline_passes_before_deletion(self):
        self.assertTrue(preflight(*self.baseline())['passed'])

    def test_ready_node_does_not_hide_publishing_target_or_survivor_lease(self):
        for target in [True, False]:
            before, claim, uid = self.baseline()
            for cm in before['configmaps']['items']:
                lease = json.loads(cm['data']['record.json'])
                if lease['phase'] == 'Ready' and (lease['plan']['worker']['uid'] == uid) == target:
                    lease['phase'] = 'Publishing'
                    cm['data']['record.json'] = json.dumps(lease)
                    break
            result = preflight(before, claim, uid)
            self.assertTrue(result['checks']['allNodesReady'])
            self.assertFalse(result['passed'])
            self.assertFalse(result['checks']['targetGatewayLeasesReady' if target else 'otherGatewayLeasesReady'])

    def test_stale_machine_or_node_association_is_rejected_before_deletion(self):
        before, claim, uid = self.baseline()
        self.assertFalse(preflight(before, claim, 'old-machine')['passed'])
        target = next(m for m in before['machines']['items'] if m['metadata']['name'] == claim)
        target['spec']['providerID'] = 'aws:///zone/i-replacement'
        self.assertFalse(preflight(before, claim, uid)['checks']['nodeAssociation'])

    def test_deleting_machine_invalidates_baseline(self):
        before, claim, uid = self.baseline()
        before['machines']['items'][0]['metadata']['deletionTimestamp'] = '2026-09-08T00:00:00Z'
        self.assertFalse(preflight(before, claim, uid)['passed'])

    def test_retired_target_attachment_is_not_a_ready_baseline(self):
        before, claim, uid = self.baseline()
        for cm in before['configmaps']['items']:
            lease = json.loads(cm['data']['record.json'])
            if lease['plan']['worker']['uid'] == uid:
                lease['phase'] = 'Complete'
                cm['data']['record.json'] = json.dumps(lease)
        self.assertFalse(preflight(before, claim, uid)['checks']['targetGatewayLeasesReady'])

    def test_gateway_withdrawal_does_not_prove_instance_termination(self):
        result = self.observe()
        self.assertTrue(result['checks']['targetGatewayLeasesComplete'])
        self.assertFalse(result['checks']['instanceTerminated'])
        self.assertFalse(result['passed'])

    def test_native_completed_removal_and_converged_survivors_pass(self):
        f = json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-removal-complete.json').read_text())
        result = self.observe(f)
        self.assertTrue(result['passed'])
        self.assertEqual(result['survivorCount'], 12)
        self.assertEqual(result['preservedGatewayLeaseCount'], 4)
        # Native snapshots immediately after termination still showed Publishing
        # for surviving attachments. Those acknowledgements must finish first.
        for cm in f['after']['configmaps']['items']:
            lease = json.loads(cm['data']['record.json'])
            if lease['phase'] == 'Ready':
                lease['phase'] = 'Publishing'
                cm['data']['record.json'] = json.dumps(lease)
                break
        self.assertFalse(self.observe(f)['passed'])

    def test_surviving_machine_and_completed_lease_cannot_be_replaced(self):
        f = json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-removal-complete.json').read_text())
        changed = copy.deepcopy(f)
        changed['after']['machines']['items'][0]['metadata']['uid'] = 'replacement'
        self.assertFalse(self.observe(changed)['checks']['survivorMachineIdentities'])
        for cm in f['after']['configmaps']['items']:
            lease = json.loads(cm['data']['record.json'])
            if lease['plan']['worker']['nodeUID'] == f['receipt']['nodeUID']:
                cm['metadata']['uid'] = 'replacement'
                break
        self.assertFalse(self.observe(f)['checks']['targetGatewayLeasesComplete'])

    def test_replacement_identity_cannot_match_old_removal_receipt(self):
        for field in ['nodeUID', 'providerID', 'instanceID']:
            f = copy.deepcopy(self.fixture)
            f['receipt'][field] = 'replacement'
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.observe(f)

    def test_survivor_replacement_or_notready_cannot_pass(self):
        for change in ['uid', 'ready', 'missing']:
            f = copy.deepcopy(self.fixture)
            nodes = f['after']['nodes']['items']
            node = next(n for n in nodes if n['metadata']['name'] == 'aws-win2022-gpu')
            if change == 'uid':
                node['metadata']['uid'] = 'replacement'
            elif change == 'missing':
                nodes.remove(node)
            else:
                next(c for c in node['status']['conditions'] if c['type'] == 'Ready')['status'] = 'False'
            with self.subTest(change=change):
                self.assertFalse(self.observe(f)['checks']['survivorIdentitiesAndReadiness'])

    def test_another_active_lease_cannot_be_withdrawn(self):
        f = copy.deepcopy(self.fixture)
        for cm in f['after']['configmaps']['items']:
            lease = json.loads(cm['data']['record.json'])
            if lease['phase'] == 'Ready':
                lease['phase'] = 'Complete'
                cm['data']['record.json'] = json.dumps(lease)
                break
        self.assertFalse(self.observe(f)['checks']['otherGatewayLeasesPreserved'])


if __name__ == '__main__':
    unittest.main()
