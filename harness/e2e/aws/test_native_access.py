import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location("native_access", pathlib.Path(__file__).with_name("native_access.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class NativeAccessTest(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
