import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("native_access", pathlib.Path(__file__).with_name("native_access.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

STATE = dict(region='us-west-2', account='123456789012', runID='cldt-test')


class NativeAccessTest(unittest.TestCase):
    def test_family_change_rejects_before_any_external_operation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'resources.json'
            original = json.dumps({'runID': 'cldt-test', 'nativeCNI': {'family': 'ipv4'}})
            path.write_text(original)
            with patch('sys.argv', ['native_access.py', '--work-dir', directory, '--family', 'ipv6']), patch.object(module.subprocess, 'run') as run:
                with self.assertRaisesRegex(RuntimeError, 'separate native CNI campaign'):
                    module.main()
                run.assert_not_called()
            self.assertEqual(path.read_text(), original)

    def test_the_policy_is_the_published_one_for_the_runs_cluster(self):
        """The harness attaches docs/iam's policy, not one of its own, keyed on
        the tag CAPA puts on the run cluster's interfaces."""
        for family, actions in (('ipv4', ['ec2:AssignPrivateIpAddresses', 'ec2:UnassignPrivateIpAddresses']),
                                ('ipv6', ['ec2:AssignIpv6Addresses', 'ec2:UnassignIpv6Addresses'])):
            policy = module.node_policy(STATE, family)
            describe = next(s for s in policy['Statement'] if s.get('Sid') == 'DescribeForIPAM')
            self.assertIn('ec2:DescribeVpcs', describe['Action'])
            self.assertIn('ec2:DescribeSecurityGroups', describe['Action'])
            mutations = [s for s in policy['Statement'] if any(not a.startswith('ec2:Describe') for a in s['Action'])]
            self.assertEqual(mutations[0]['Action'], actions)
            for statement in mutations:
                self.assertEqual(statement['Resource'], 'arn:aws:ec2:us-west-2:123456789012:network-interface/*')
                self.assertEqual(statement['Condition']['StringEquals'], {
                    'aws:RequestedRegion': 'us-west-2',
                    'ec2:ResourceTag/sigs.k8s.io/cluster-api-provider-aws/cluster/cldt-test': 'owned'})
            tagging = next(s for s in mutations if s['Action'] == ['ec2:CreateTags'])
            self.assertEqual(tagging['Condition']['ForAllValues:StringEquals']['aws:TagKeys'], ['node.k8s.amazonaws.com/instance_id'])

    def test_self_rule_is_detected(self):
        group = {'GroupId': 'sg-1', 'IpPermissions': [{'IpProtocol': 'udp', 'UserIdGroupPairs': [{'GroupId': 'sg-1'}]}]}
        self.assertFalse(module.self_admitting(group))
        group['IpPermissions'].append({'IpProtocol': '-1', 'UserIdGroupPairs': [{'GroupId': 'sg-1'}]})
        self.assertTrue(module.self_admitting(group))


if __name__ == "__main__":
    unittest.main()
