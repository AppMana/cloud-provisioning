import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("native_access", pathlib.Path(__file__).with_name("native_access.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class NativeAccessTest(unittest.TestCase):
    def test_family_change_rejects_before_any_external_operation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'resources.json'
            original = json.dumps({'runID': 'cldt-test', 'nativeCNI': {'family': 'ipv4'}})
            path.write_text(original)
            with patch('sys.argv', ['native_access.py', '--work-dir', directory, '--runner', 'test', '--family', 'ipv6']), patch.object(module.subprocess, 'run') as run:
                with self.assertRaisesRegex(RuntimeError, 'separate native CNI campaign'):
                    module.main()
                run.assert_not_called()
            self.assertEqual(path.read_text(), original)

    def test_ipv6_ipam_keeps_ownership_scope_and_cannot_assign_ipv4(self):
        state = dict(region='us-west-2', account='123456789012', runID='cldt-test')
        v4 = module.node_policy(state)
        v6 = module.node_policy(state, 'ipv6')
        self.assertEqual(v6['Statement'][1]['Action'], ['ec2:AssignIpv6Addresses', 'ec2:UnassignIpv6Addresses'])
        v6['Statement'][1]['Action'] = v4['Statement'][1]['Action']
        self.assertEqual(v4, v6)

    def test_ipv6_scope_uses_only_the_associated_subnet_range(self):
        subnet = {'CidrBlock': '172.29.0.0/25', 'Ipv6CidrBlockAssociationSet': [
            {'Ipv6CidrBlock': '2001:db8:1::/64', 'Ipv6CidrBlockState': {'State': 'associated'}},
            {'Ipv6CidrBlock': '2001:db8:2::/64', 'Ipv6CidrBlockState': {'State': 'disassociated'}}]}
        self.assertEqual(module.subnet_pod_range(subnet, 'ipv4'), '172.29.0.0/25')
        self.assertEqual(module.subnet_pod_range(subnet, 'ipv6'), '2001:db8:1::/64')
        subnet['Ipv6CidrBlockAssociationSet'][0]['Ipv6CidrBlockState']['State'] = 'associating'
        with self.assertRaises(ValueError):
            module.subnet_pod_range(subnet, 'ipv6')

    def test_ipam_mutations_are_limited_to_owned_interfaces(self):
        policy = module.node_policy(dict(region="us-west-2", account="123456789012", runID="cldt-test"))
        mutations = [s for s in policy["Statement"] if any(not a.startswith("ec2:Describe") for a in s["Action"])]
        self.assertEqual(len(mutations), 2)
        statement = mutations[0]
        self.assertEqual(set(statement["Action"]), {"ec2:AssignPrivateIpAddresses", "ec2:UnassignPrivateIpAddresses"})
        self.assertEqual(statement["Resource"], "arn:aws:ec2:us-west-2:123456789012:network-interface/*")
        self.assertEqual(statement["Condition"]["StringEquals"], {
            "aws:RequestedRegion": "us-west-2",
            "ec2:ResourceTag/cloud-provisioning-test": "cldt-test",
        })

    def test_tagging_cannot_change_campaign_ownership(self):
        policy = module.node_policy(dict(region="us-west-2", account="123456789012", runID="cldt-test"))
        tagging = next(s for s in policy["Statement"] if s["Action"] == ["ec2:CreateTags"])
        self.assertEqual(tagging["Condition"]["StringEquals"]["ec2:ResourceTag/cloud-provisioning-test"], "cldt-test")
        self.assertEqual(tagging["Condition"]["ForAllValues:StringEquals"]["aws:TagKeys"], ["node.k8s.amazonaws.com/instance_id"])

    def test_site_exclusion_neither_allocates_nor_advertises_aws_addresses(self):
        obj = module.calico_exclusion({'runID': 'cldt-test'}, '172.29.0.0/25')
        self.assertEqual(obj['metadata']['labels']['cloud-provisioning-test'], 'cldt-test')
        self.assertEqual(obj['spec']['cidr'], '172.29.0.0/25')
        self.assertTrue(obj['spec']['disabled'])
        self.assertTrue(obj['spec']['disableBGPExport'])
        self.assertEqual(obj['spec']['nodeSelector'], '!all()')
        self.assertFalse(obj['spec']['natOutgoing'])


if __name__ == "__main__":
    unittest.main()
