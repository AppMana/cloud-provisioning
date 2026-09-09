import copy
import unittest
import json
import pathlib
from replacement import evaluate,FRESH


class ReplacementTest(unittest.TestCase):
    def setUp(self):
        self.before=dict(claim='worker',templateUID='template',eniCount=1,desiredPeerSHA256='old-hash',appliedPeerSHA256='old-hash',**{key:'old-'+key for key in FRESH})
        self.after=dict(claim='worker',templateUID='template',eniCount=1,desiredPeerSHA256='new-hash',appliedPeerSHA256='new-hash',**{key:'new-'+key for key in FRESH})
        self.removal={key:self.before[key] for key in ['claim','nodeUID','instanceID','providerID']}
        self.removal.update(terminated=True,productCleanup=True)

    def test_full_identity_replacement(self):
        self.assertTrue(evaluate(self.before,self.after,self.removal)['passed'])

    def test_reused_or_missing_identity_cannot_pass(self):
        for key in FRESH:
            for value in [self.before[key],'',42,{'not':'identity'}]:
                with self.subTest(key=key,value=value):
                    after=copy.deepcopy(self.after);after[key]=value
                    self.assertFalse(evaluate(self.before,after,self.removal)['passed'])

    def test_stale_receipt_and_wrong_retirement_rejected(self):
        after=dict(self.after,appliedPeerSHA256=self.before['appliedPeerSHA256'])
        self.assertFalse(evaluate(self.before,after,self.removal)['passed'])
        for key in ['nodeUID','instanceID','providerID','claim']:
            removal=dict(self.removal);removal[key]='other'
            self.assertFalse(evaluate(self.before,self.after,removal)['passed'])
        self.assertFalse(evaluate(self.before,self.after,dict(self.removal,terminated=False))['passed'])

    def test_different_claim_template_or_multiple_nics_rejected(self):
        for patch in [dict(claim='other'),dict(templateUID='other'),dict(eniCount=2),dict(eniCount=True)]:
            self.assertFalse(evaluate(self.before,dict(self.after,**patch),self.removal)['passed'])

    def test_native_identical_content_requires_new_secret_identity(self):
        native=json.loads((pathlib.Path(__file__).parent/'testdata/capi-linux-replacement.json').read_text())
        self.assertTrue(evaluate(native['before'],native['after'],native['removal'])['passed'])
        self.assertEqual(native['before']['desiredPeerSHA256'],native['after']['desiredPeerSHA256'])
        after=dict(native['after'],adoptionSecretUID=native['before']['adoptionSecretUID'])
        self.assertFalse(evaluate(native['before'],after,native['removal'])['passed'])
