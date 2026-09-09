import copy
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from gateway_request import binding, request_configmap, main


class NativeGatewayBinding(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((pathlib.Path(__file__).parent/'testdata/gateway-native-binding.json').read_text())

    def bind(self, role, fixture=None):
        f = fixture or self.fixture
        r = f[role]
        return binding(f['state'], r['machine'], r['node'], r['instance'], f['subnet'], r['expectedUID'])

    def test_same_model_binds_windows_worker_and_linux_gateway(self):
        worker = self.bind('worker')
        gateway = self.bind('gateway')
        self.assertEqual(gateway, self.fixture['template']['gateway'])
        cm = request_configmap(self.fixture['template'], worker, gateway, 'aws-win2025-gpu', ['203.0.113.2'])
        request = json.loads(cm['data']['request.json'])
        self.assertEqual(request['worker'], worker)
        self.assertEqual(request['tcpPorts'], [8132])
        self.assertEqual(request['gateway'], gateway)
        self.assertNotEqual(request['worker'], self.fixture['template']['worker'])

    def test_replacement_and_deleting_identities_are_rejected(self):
        for change in ['machineUID', 'nodeProvider', 'instanceID', 'deleting']:
            f = copy.deepcopy(self.fixture)
            r = f['worker']
            if change == 'machineUID': r['machine']['metadata']['uid'] = 'replacement'
            elif change == 'nodeProvider': r['node']['spec']['providerID'] = 'aws:///us-west-2a/i-other'
            elif change == 'instanceID': r['instance']['InstanceId'] = 'i-other'
            else: r['machine']['metadata']['deletionTimestamp'] = '2026-09-07T18:31:08Z'
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.bind('worker', f)

    def test_wrong_ownership_subnet_or_multiple_nics_are_rejected(self):
        for change in ['owner', 'subnet', 'nics', 'deviceIndex', 'address']:
            f = copy.deepcopy(self.fixture)
            instance = f['worker']['instance']
            if change == 'owner': instance['Tags'] = []
            elif change == 'subnet': instance['SubnetId'] = 'subnet-other'
            elif change == 'nics': instance['NetworkInterfaces'] *= 2
            elif change == 'deviceIndex': instance['NetworkInterfaces'][0]['Attachment']['DeviceIndex'] = 1
            else: instance['NetworkInterfaces'][0]['PrivateIpAddress'] = '192.0.2.1'
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.bind('worker', f)

    def test_replaced_gateway_template_and_self_gateway_are_rejected(self):
        worker, gateway = self.bind('worker'), self.bind('gateway')
        template = copy.deepcopy(self.fixture['template'])
        template['gateway']['uid'] = 'replacement'
        with self.assertRaises(ValueError):
            request_configmap(template, worker, gateway, 'worker', [])
        with self.assertRaises(ValueError):
            request_configmap(self.fixture['template'], gateway, gateway, 'worker', [])

    def exercise_cli(self, drift=False):
        f = self.fixture
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root/'resources.json').write_text(json.dumps(f['state']))
            (root/'harness-session.json').write_text(json.dumps({'Credentials':{
                'AccessKeyId':'fixture','SecretAccessKey':'fixture','SessionToken':'fixture'}}))
            intent = root/'intent.json'
            created = []
            reads = 0

            def read(args, **kwargs):
                nonlocal reads
                start = args.index('get')
                kind = args[start+1]
                name = args[start+2]
                if kind == 'configmap': return json.dumps({'data':{'request.json':json.dumps(f['template'])}})
                if kind == 'machines': return json.dumps({'items':f['underlayMachines']})
                role = 'worker' if name == f['worker']['machine']['metadata']['name'] else 'gateway'
                result = copy.deepcopy(f[role][kind])
                if kind == 'machine' and role == 'worker':
                    reads += 1
                    if drift and reads == 2: result['metadata']['uid'] = 'replacement'
                return json.dumps(result)

            def run(args, **kwargs):
                if args[0] == 'aws':
                    operation = args[4]
                    inputs = json.loads(args[args.index('--cli-input-json')+1])
                    if operation == 'describe-subnets': body = {'Subnets':[f['subnet']]}
                    else:
                        instance = next(f[role]['instance'] for role in ['worker','gateway'] if f[role]['instance']['InstanceId'] == inputs['InstanceIds'][0])
                        body = {'Reservations':[{'Instances':[instance]}]}
                    return subprocess.CompletedProcess(args,0,json.dumps(body),'')
                # A durable, exact intent must exist before the external mutation.
                self.assertTrue(intent.exists())
                self.assertEqual(json.loads(intent.read_text()),json.loads(kwargs['input']))
                created.append(json.loads(kwargs['input']))
                return subprocess.CompletedProcess(args,0,'created','')

            argv = ['gateway_request.py','--work-dir',str(root),'--intent',str(intent),
                    '--api-server','https://10.10.0.10:6443',
                    '--worker',f['worker']['machine']['metadata']['name'],'--worker-uid',f['worker']['expectedUID'],
                    '--gateway',f['gateway']['machine']['metadata']['name'],'--gateway-uid',f['gateway']['expectedUID'],
                    '--template-configmap','gateway-template']
            with patch.object(sys,'argv',argv), patch('subprocess.check_output',read), patch('subprocess.run',run):
                if drift:
                    with self.assertRaises(ValueError): main()
                    self.assertFalse(intent.exists())
                    self.assertEqual(created,[])
                else:
                    main()
                    self.assertEqual(len(created),1)
                    request = json.loads(created[0]['data']['request.json'])
                    # Native fake-VM workers use a different CAPI Cluster record
                    # while sharing the AWS workers' workload site and tunnels.
                    self.assertIn('203.0.113.10',request['underlay'])
                    self.assertIn('192.0.2.10',request['underlay'])
                    # A second invocation must not overwrite or re-submit it.
                    original = intent.read_bytes()
                    with self.assertRaises(SystemExit): main()
                    self.assertEqual(intent.read_bytes(),original)
                    self.assertEqual(len(created),1)

    def test_cli_records_intent_before_create_and_refuses_replay(self):
        self.exercise_cli()

    def test_cli_rechecks_replacement_identity_before_submission(self):
        self.exercise_cli(drift=True)


if __name__ == '__main__':
    unittest.main()
