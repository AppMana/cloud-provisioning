import copy
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch
from windows_first_application import qualify, submit, validate_job


class FirstApplicationTest(unittest.TestCase):
    def setUp(self):
        self.data = json.loads((pathlib.Path(__file__).parent/'testdata/windows-first-application-native.json').read_text())
        self.node = self.data['beforeNode']
        self.name = self.node['metadata']['name']
        self.uid = self.node['metadata']['uid']
        self.job = self.data['job']
        self.creates = []
        self.reads = 0
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.evidence = pathlib.Path(self.temp.name)/'trial'

    def get(self, kind, name, namespace):
        if kind == 'node':
            self.reads += 1
            return self.node
        return self.data['beforePods'] if kind == 'pods' else self.data['daemonsets']

    def create(self, job):
        self.creates.append(job)
        return dict(job, metadata=dict(job['metadata'], uid='created-job-uid'))

    def run_trial(self, **kwargs):
        return submit(self.job, self.name, self.uid, self.evidence, self.get, self.create, **kwargs)

    def test_native_plugin_ready_but_resource_not_published(self):
        result = qualify(self.node, self.data['beforePods'], self.data['daemonsets'], self.uid)
        self.assertTrue(result['placement']['passed'])
        self.assertFalse(result['startup']['checks']['gpuReportedAllocatable'])
        with self.assertRaises(TimeoutError):
            self.run_trial(timeout=0)
        self.assertEqual(self.creates, [])
        self.assertTrue((self.evidence/'observation-0000.json').exists())

    def test_observed_resource_transition_waits_then_creates_once(self):
        def register(_):
            self.node = self.data['afterNode']  # Replay native pre-submission resource registration.
        with patch('windows_first_application.time.sleep', side_effect=register):
            created = self.run_trial()
        self.assertEqual(len(self.creates), 1)
        self.assertEqual(created['metadata']['uid'], 'created-job-uid')
        self.assertTrue((self.evidence/'observation-0001.json').exists())
        with self.assertRaises(FileExistsError):
            self.run_trial()
        self.assertEqual(len(self.creates), 1)

    def test_prior_ordinary_container_fails_without_wait_or_creation(self):
        pod = self.data['beforePods']['items'][0]
        pod['spec']['ephemeralContainers'] = [{'name': 'debug', 'securityContext': {'windowsOptions': {'hostProcess': False}}}]
        with self.assertRaises(ValueError):
            self.run_trial()
        self.assertEqual(self.creates, [])

    def test_node_replacement_between_guard_and_create_is_rejected(self):
        self.node = self.data['afterNode']
        original = self.get
        def get(*args):
            node = original(*args)
            if args[0] == 'node' and self.reads == 2:
                node = copy.deepcopy(node)
                node['metadata']['uid'] = 'replacement'
            return node
        self.get = get
        with self.assertRaises(ValueError):
            self.run_trial()
        self.assertEqual(self.creates, [])

    def test_ordinary_workload_arriving_during_final_check_prevents_create(self):
        self.node = self.data['afterNode']
        original = self.get
        def get(*args):
            result = original(*args)
            if args[0] == 'pods' and self.reads == 2:
                result = copy.deepcopy(result)
                result['items'][0]['spec']['ephemeralContainers'] = [
                    {'name': 'late-debug', 'securityContext': {'windowsOptions': {'hostProcess': False}}}]
            return result
        self.get = get
        with self.assertRaises(ValueError):
            self.run_trial()
        self.assertEqual(self.creates, [])

    def test_ambiguous_create_is_retained_and_never_retried(self):
        self.node = self.data['afterNode']
        def ambiguous(job):
            self.creates.append(job)
            raise TimeoutError('response lost after API acceptance')
        self.create = ambiguous
        with self.assertRaises(TimeoutError):
            self.run_trial()
        self.assertEqual(len(self.creates), 1)
        self.assertTrue((self.evidence/'create-unresolved.json').exists())
        with self.assertRaises(FileExistsError):
            self.run_trial()
        self.assertEqual(len(self.creates), 1)

    def test_job_cannot_retry_autodelete_or_target_another_node(self):
        for key, value in [('backoffLimit', 1), ('completions', 2), ('parallelism', 2),
                           ('ttlSecondsAfterFinished', 0), ('suspend', True)]:
            with self.subTest(key=key):
                job = copy.deepcopy(self.job); job['spec'][key] = value
                with self.assertRaises(ValueError): validate_job(job, self.name)
        with self.assertRaises(ValueError): validate_job(self.job, 'different-node')
        self.job['spec']['template']['spec']['containers'][0]['image'] = 'probe:latest'
        with self.assertRaises(ValueError): validate_job(self.job, self.name)


if __name__ == '__main__':
    unittest.main()
