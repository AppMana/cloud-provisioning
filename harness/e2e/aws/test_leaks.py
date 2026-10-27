import unittest

from leaks import product_leaks

STATE = {'runID': 'cldt-test', 'routeTableID': 'rtb-1', 'securityGroupID': 'sg-1', 'vpcID': 'vpc-1'}


def fake(responses):
    def aws(service, operation, *args, absent_ok=False):
        return responses.get(operation, {})
    return aws


class LeakTest(unittest.TestCase):
    def test_a_clean_run_has_no_leaks(self):
        aws = fake({'describe-route-tables': {'RouteTables': [{'Routes': [
            {'DestinationCidrBlock': '172.29.0.0/16', 'GatewayId': 'local'},
            {'DestinationCidrBlock': '0.0.0.0/0', 'GatewayId': 'igw-1'},
            {'DestinationIpv6CidrBlock': '::/0', 'GatewayId': 'igw-1'}]}]},
            'describe-security-group-rules': {'SecurityGroupRules': [{'SecurityGroupRuleId': 'sgr-1', 'Tags': []}]}})
        self.assertEqual(product_leaks(aws, STATE), [])

    def test_what_the_product_left_is_reported(self):
        aws = fake({'describe-route-tables': {'RouteTables': [{'Routes': [
            {'DestinationCidrBlock': '10.101.128.5/32', 'NetworkInterfaceId': 'eni-gw'}]}]},
            'describe-security-group-rules': {'SecurityGroupRules': [
                {'SecurityGroupRuleId': 'sgr-2', 'Tags': [{'Key': 'cloud-provisioning.appmana.com/ingress-owner', 'Value': 'lease'}]}]},
            'describe-network-interfaces': {'NetworkInterfaces': [{'NetworkInterfaceId': 'eni-left', 'Status': 'available'}]},
            'list-secrets': {'SecretList': [{'ARN': 'arn:aws:secretsmanager:r:1:secret:aws.cluster.x-k8s.io/x'}]}})
        kinds = sorted(leak['kind'] for leak in product_leaks(aws, STATE))
        self.assertEqual(kinds, ['bootstrap-secret', 'ingress-rule', 'network-interface', 'route'])


if __name__ == '__main__':
    unittest.main()
