import unittest

import prepare_campaign as subject

DIGEST = 'sha256:' + 'a' * 64
CONTROLLER = '123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/endpoint-controller@' + DIGEST
DIALER = '123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/linux-dialer@sha256:' + 'b' * 64


def state(**overrides):
    value = {'runID': 'cldt-test', 'linuxControllerImage': CONTROLLER, 'linuxControllerPullSecret': 'cldt-test-controller-pull',
             'linuxDialerImage': DIALER, 'linuxDialerPullSecret': 'cldt-test-dialer-pull'}
    value.update(overrides)
    return value


URLS = {'wg-dialer-linux-amd64': 'https://signed/amd64', 'wg-dialer-linux-arm64': 'https://signed/arm64'}
BINARIES = {'amd64': '1' * 64, 'arm64': '2' * 64}


class RenderValuesTest(unittest.TestCase):
    def test_values_bind_published_digests_and_receipt_policy(self):
        values = subject.render_values(state(), URLS, BINARIES, ['203.0.113.0/24', '172.29.0.0/25'])
        self.assertEqual(values['image'], {'repository': CONTROLLER.split('@')[0], 'tag': '', 'digest': DIGEST,
                                           'pullPolicy': 'IfNotPresent'})
        self.assertEqual(values['dialerImage']['digest'], DIALER.split('@')[1])
        self.assertEqual([s['name'] for s in values['imagePullSecrets']],
                         ['cldt-test-controller-pull', 'cldt-test-dialer-pull'])
        self.assertEqual(values['dialerBinary']['arm64'], {'url': 'https://signed/arm64', 'sha256': '2' * 64})
        receipts = values['tunnel']['receivePreparation']['journal']['receipts']
        # Every architecture admits exactly the deployed dialer reference.
        self.assertEqual(receipts['imageIDs'], {'amd64': DIALER, 'arm64': DIALER})
        self.assertEqual(receipts['certFile'], '/etc/cloud-provisioning/receipts/tls.crt')
        self.assertNotIn('endpoint', receipts)
        # The chart issues the receipt certificate; the harness names none.
        self.assertNotIn('receiptTLSSecret', values['tunnel'])
        self.assertEqual(values['tunnel']['receivePreparation']['nativeCIDRs'], ['203.0.113.0/24', '172.29.0.0/25'])
        self.assertEqual(values['aws'], {'credentials': {'secretName': 'cloud-provisioning-aws', 'mount': 'env'}})
        self.assertNotIn('awsPodObserver', values)

    def test_unpublished_component_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'linuxDialer has not been published'):
            subject.render_values(state(linuxDialerPullSecret=''), URLS, BINARIES, ['203.0.113.0/24'])

    def test_mutable_image_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'immutable'):
            subject.render_values(state(linuxControllerImage='repo/controller@latest'), URLS, BINARIES, ['203.0.113.0/24'])

    def test_missing_architecture_binary_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'arm64'):
            subject.render_values(state(), {'wg-dialer-linux-amd64': 'u'}, BINARIES, ['203.0.113.0/24'])

    def test_receiver_preparation_requires_cloud_ranges(self):
        with self.assertRaisesRegex(ValueError, 'protected cloud range'):
            subject.render_values(state(), URLS, BINARIES, [])


class ControllerCredentials(unittest.TestCase):
    def test_the_secret_holds_exactly_the_sdk_keys(self):
        from prepare_campaign import aws_secret
        keys = {'AWS_ACCESS_KEY_ID': 'AKIA', 'AWS_SECRET_ACCESS_KEY': 'secret', 'AWS_REGION': 'us-west-2'}
        secret = aws_secret(keys)
        self.assertEqual(secret['metadata'], {'name': 'cloud-provisioning-aws', 'namespace': 'cloud-provisioning'})
        self.assertEqual(secret['stringData'], keys)
        with self.assertRaises(ValueError):
            aws_secret(dict(keys, AWS_SESSION_TOKEN='expiring'))


if __name__ == '__main__':
    unittest.main()
