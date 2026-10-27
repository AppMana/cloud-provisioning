import base64
import unittest

import identity


class CAPAIdentityTest(unittest.TestCase):
    def test_capa_assumes_its_role_from_a_long_lived_source(self):
        state = {'runID': 'cldt-test', 'capaRoleARN': 'arn:aws:iam::123456789012:role/cldt-test-capa'}
        secret, static, role = identity.objects(state, {'AccessKeyID': 'AKIA', 'SecretAccessKey': 'secret'})
        self.assertEqual(sorted(secret['data']), ['AccessKeyID', 'SecretAccessKey'])
        self.assertEqual(base64.b64decode(secret['data']['AccessKeyID']).decode(), 'AKIA')
        self.assertEqual(static['spec']['secretRef'], secret['metadata']['name'])
        self.assertEqual(role['kind'], 'AWSClusterRoleIdentity')
        self.assertEqual(role['metadata']['name'], 'cldt-test')
        self.assertEqual(role['spec']['roleARN'], state['capaRoleARN'])
        self.assertEqual(role['spec']['sourceIdentityRef'], {'kind': 'AWSClusterStaticIdentity', 'name': static['metadata']['name']})


if __name__ == '__main__':
    unittest.main()
