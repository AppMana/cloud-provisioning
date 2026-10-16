import base64
import copy
import datetime as dt
import json
import unittest

import observer_credentials as subject


class FakeCommands:
    def __init__(self, mode='create'):
        self.mode = mode
        self.calls = []
        self.writes = []
        self.identity_reads = 0
        self.secret = None
        if mode in ('replace', 'foreign', 'ambiguous'):
            self.secret = {'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
                'metadata': {'name': subject.SECRET, 'namespace': subject.NAMESPACE,
                    'uid': 'secret-uid', 'resourceVersion': '41',
                    'labels': {subject.OWNER: 'foreign' if mode == 'foreign' else 'cldt-test'}},
                'data': {'unrelated': 'preserved'}}

    def __call__(self, argv, body=None):
        self.calls.append(argv)
        if 'kubectl' in argv:
            assert not any(arg.startswith('--server') for arg in argv), 'use the selected lab kubeconfig endpoint'
        if argv[:2] == ['docker', 'inspect']:
            return json.dumps([{'HostConfig': {'NetworkMode': 'host' if self.mode == 'host-network' else 'bridge',
                                             'PidMode': ''}, 'Mounts': []}])
        if 'info' in argv:
            return 'outer' if argv[:2] == ['docker', 'info'] or self.mode == 'same-daemon' else 'inner'
        if 'get-caller-identity' in argv:
            return json.dumps({'Account': 'wrong' if self.mode == 'wrong-account' else '123456789012'})
        if 'get-federation-token' in argv:
            policy = json.loads(argv[argv.index('--policy') + 1])
            assert policy == subject.policy('us-west-2')
            expiry = dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=-1 if self.mode == 'expired' else 60)
            return json.dumps({'Credentials': {'AccessKeyId': 'temporary-key', 'SecretAccessKey': 'private-secret',
                'SessionToken': 'private-token', 'Expiration': expiry.isoformat()}})
        if 'namespace' in argv:
            self.identity_reads += 1
            uid = 'wrong' if self.mode == 'wrong-cluster' or (self.mode == 'changed-cluster' and self.identity_reads == 2) else 'cluster-uid'
            return json.dumps({'metadata': {'uid': uid}})
        if 'get' in argv:
            return json.dumps(self.secret) if self.secret else ''
        if 'create' in argv or 'replace' in argv:
            self.writes.append(json.loads(body))
            if self.mode == 'ambiguous':
                raise RuntimeError('ambiguous write')
            obj = copy.deepcopy(self.writes[-1])
            obj['metadata'].update(uid='secret-uid', resourceVersion='42')
            return json.dumps(obj)
        raise AssertionError('unexpected external command')


class ObserverRenewalTests(unittest.TestCase):
    state = {'runID': 'cldt-test', 'region': 'us-west-2', 'account': '123456789012'}

    def test_create_and_replace_only_scoped_secret(self):
        for mode in ['create', 'replace']:
            with self.subTest(mode=mode):
                calls = FakeCommands(mode)
                receipt = subject.renewal(self.state, 'isolated-runner', 'cluster-uid', calls)
                self.assertEqual(len(calls.writes), 1)
                self.assertEqual(receipt['operation'], mode)
                self.assertNotIn('private-', json.dumps(receipt))
                obj = calls.writes[0]
                self.assertEqual(obj['metadata']['name'], subject.SECRET)
                credentials = json.loads(base64.b64decode(obj['data']['credentials.json']))
                self.assertEqual(credentials['SessionToken'], 'private-token')
                if mode == 'replace':
                    self.assertEqual(obj['metadata']['resourceVersion'], '41')
                    self.assertEqual(obj['data']['unrelated'], 'preserved')
                self.assertFalse(any('rollout' in argv or 'iam' in argv or 'apply' in argv for argv in calls.calls))

    def test_refuses_wrong_scope_before_any_secret_write(self):
        for mode in ['foreign', 'host-network', 'same-daemon', 'wrong-cluster', 'changed-cluster', 'wrong-account', 'expired']:
            with self.subTest(mode=mode):
                calls = FakeCommands(mode)
                with self.assertRaises(ValueError):
                    subject.renewal(self.state, 'isolated-runner', 'cluster-uid', calls)
                self.assertEqual(calls.writes, [])

    def test_ambiguous_write_is_not_retried(self):
        calls = FakeCommands('ambiguous')
        with self.assertRaises(RuntimeError):
            subject.renewal(self.state, 'isolated-runner', 'cluster-uid', calls)
        self.assertEqual(len(calls.writes), 1)

    def test_cleaned_run_never_calls_external_tools(self):
        calls = FakeCommands()
        with self.assertRaises(ValueError):
            subject.renewal(dict(self.state, cleanedUp=True), 'isolated-runner', 'cluster-uid', calls)
        self.assertEqual(calls.calls, [])


if __name__ == '__main__':
    unittest.main()
