import copy
import unittest

from windows_token import ANNOTATION, acknowledged, rotated


class RotationTest(unittest.TestCase):
    def test_observed_rotation_after_requested_lifetime(self):
        container = {'restartCount': 0, 'state': {'running': {'startedAt': '2026-09-07T04:55:45Z'}}}
        # Sanitized shape of the real Windows HostProcess volume observation.
        from windows_token import timestamp
        start = timestamp(container['state']['running']['startedAt'])
        metadata = {'issuedAt': start + 2900, 'expiresAt': start + 6500}
        self.assertTrue(rotated(container, metadata, start + 4200, 3600))
        for bad in [{'issuedAt': start - 1, 'expiresAt': start + 6500},
                    {'issuedAt': start + 5000, 'expiresAt': start + 6500},
                    {'issuedAt': start + 2900, 'expiresAt': start + 4000}]:
            self.assertFalse(rotated(container, bad, start + 4200, 3600))
        self.assertFalse(rotated(container, metadata, start + 3500, 3600))
        container['restartCount'] = 1
        self.assertFalse(rotated(container, metadata, start + 4200, 3600))

    def test_only_fresh_same_identity_same_payload_acknowledges(self):
        import base64
        import hashlib
        payload = b'{"peers":[]}'
        before = {'metadata': {'uid': 'original', 'resourceVersion': '100'},
                  'data': {'peers.json': base64.b64encode(payload).decode()}}
        after = copy.deepcopy(before)
        after['metadata'].update(resourceVersion='102', annotations={ANNOTATION: hashlib.sha256(payload).hexdigest()})
        self.assertTrue(acknowledged(before, after))
        for field, value in [('uid', 'replacement'), ('resourceVersion', '100'), ('annotations', {})]:
            bad = copy.deepcopy(after)
            bad['metadata'][field] = value
            self.assertFalse(acknowledged(before, bad))
        after['data']['peers.json'] = base64.b64encode(b'{"peers":[{}]}').decode()
        self.assertFalse(acknowledged(before, after))


if __name__ == '__main__':
    unittest.main()
