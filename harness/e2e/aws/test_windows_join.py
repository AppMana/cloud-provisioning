"""CAPI v1beta2 association contract observed on the real VM site."""
import unittest
import base64
import copy
import datetime
import hashlib
from windows_join import associated, delivered, image_artifacts, record_guest_failure, gateway_ready
import pathlib
import tempfile
import types

class GatewayReadinessTest(unittest.TestCase):
    def test_native_gateway_attachment_transition(self):
        import json
        observed=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gateway-readiness.json').read_text())
        self.assertFalse(gateway_ready(observed['before'],'observed-machine','observed-node'))
        self.assertTrue(gateway_ready(observed['after'],'observed-machine','observed-node'))
        self.assertFalse(gateway_ready(observed['after'],'observed-machine','replacement-node'))

    def fixture(self, phase='Ready', node='current'):
        import json
        return {'items':[{'metadata':{'name':'network-attachment-test'},'data':{'record.json':json.dumps({'phase':phase,'plan':{'worker':{'uid':'worker','nodeUID':node}}})}}]}

    def test_absent_gateway_cannot_be_hidden_by_other_join_checks(self):
        self.assertFalse(gateway_ready({'items':[]},'worker','current'))
        self.assertFalse(gateway_ready(self.fixture(),'different-worker','current'))
        self.assertTrue(gateway_ready(self.fixture(),'worker','current'))

    def test_publishing_stale_retired_and_deleting_attachments_fail(self):
        for phase in ['Publishing','Complete','Staging']:
            self.assertFalse(gateway_ready(self.fixture(phase=phase),'worker','current'))
        self.assertFalse(gateway_ready(self.fixture(node='old'),'worker','current'))
        fixture=self.fixture();fixture['items'][0]['metadata']['deletionTimestamp']='now'
        self.assertFalse(gateway_ready(fixture,'worker','current'))

    def test_malformed_state_is_not_ready(self):
        self.assertFalse(gateway_ready({'items':[{'metadata':{'name':'network-attachment-bad'},'data':{'record.json':'broken'}}]},'worker','current'))

    def test_ready_attachment_cannot_hide_another_pending_attachment(self):
        fixture=self.fixture()
        fixture['items']+=self.fixture(phase='Publishing')['items']
        self.assertFalse(gateway_ready(fixture,'worker','current'))
        fixture=self.fixture()
        fixture['items']+=self.fixture(phase='Complete',node='old')['items']
        self.assertTrue(gateway_ready(fixture,'worker','current'))


class GuestDiagnosticTest(unittest.TestCase):
    def test_failed_executor_evidence_is_private_and_never_overwritten(self):
        with tempfile.TemporaryDirectory() as root:
            output=pathlib.Path(root)/'attempt.json'
            report={};result=types.SimpleNamespace(returncode=1,stderr='retained native executor failure')
            record_guest_failure(report,output,result)
            artifact=output.with_suffix('.guest.stderr')
            self.assertEqual(artifact.read_text(),result.stderr)
            self.assertEqual(artifact.stat().st_mode & 0o777,0o600)
            self.assertNotIn(result.stderr,str(report))
            self.assertEqual(report['guestObservation']['exitCode'],1)
            with self.assertRaises(FileExistsError):record_guest_failure({},output,result)


class ImageArtifactTest(unittest.TestCase):
    def test_running_worker_must_match_the_captured_recipe(self):
        image=dict(workerVersion='v1.36.2+k0s.0.appmana.1',workerSHA256='a'*64,tunnelSHA256='b'*64)
        guest=dict(k0sVersion=image['workerVersion'],k0sSHA256=image['workerSHA256'],tunnelSHA256=image['tunnelSHA256'],runningTunnels=[dict(name='cloud-provisioning-cldt1775f69e',processId=3644,executableSHA256=image['tunnelSHA256'])])
        self.assertTrue(all(image_artifacts(image,guest).values()))
        for key in guest:
            wrong=dict(guest);wrong[key]='different'
            self.assertFalse(all(image_artifacts(image,wrong).values()))
        self.assertFalse(any(image_artifacts({},guest).values()))

    def test_live_canary_cannot_pass_using_only_the_original_baked_file(self):
        # Native rollout kept the original baked binary and switched SCM to a
        # staged executable with a different hash.
        baked='3f9a1241788f67e56f7318464416078a68d5e0dca096f9e4f901f061859d2f71'
        candidate='a513e39abc7ab84739160d60efffc7eb873b589a26ab3e0971e4e665060e0f3a'
        guest={'tunnelSHA256':baked,'runningTunnels':[{'name':'cloud-provisioning-cldt1775f69e','processId':3652,'executableSHA256':candidate}]}
        checks=image_artifacts({'tunnelSHA256':baked},guest)
        self.assertTrue(checks['expectedTunnelHash'])
        self.assertFalse(checks['expectedRunningTunnelHash'])
        self.assertTrue(image_artifacts({'tunnelSHA256':baked},guest,candidate)['expectedRunningTunnelHash'])
        guest['runningTunnels']=[]
        self.assertFalse(image_artifacts({'tunnelSHA256':baked},guest,candidate)['expectedRunningTunnelHash'])

class AssociationTest(unittest.TestCase):
    def setUp(self):
        # nodeRef's name-only shape was read from the live remote1 Machine.
        self.machine={'spec':{'providerID':'aws:///zone/i-worker'},'status':{'phase':'Running','nodeRef':{'name':'worker'}}}
        self.node={'metadata':{'name':'worker','uid':'actual-node-uid'},'spec':{'providerID':'aws:///zone/i-worker'}}

    def test_v1beta2_reference_has_no_uid(self):
        self.assertTrue(associated(self.machine,self.node))

    def test_running_unrelated_node_is_not_associated(self):
        self.node['spec']['providerID']='aws:///zone/i-other'
        self.assertFalse(associated(self.machine,self.node))

    def test_registration_without_machine_convergence_is_incomplete(self):
        self.machine['status']['phase']='Provisioned'
        self.assertFalse(associated(self.machine,self.node))

    def test_missing_node_identity_is_incomplete(self):
        self.node['metadata'].pop('uid')
        self.assertFalse(associated(self.machine,self.node))

class DeliveryTest(unittest.TestCase):
    def setUp(self):
        # Shape observed through native SSM on both joined Windows builds.
        self.now=datetime.datetime.now(datetime.timezone.utc)
        digest=hashlib.sha256(b'{"peers":[]}').hexdigest()
        self.secret={'metadata':{'uid':'current-secret','annotations':{'cloud-provisioning.appmana.com/applied':digest}},'data':{'peers.json':base64.b64encode(b'{"peers":[]}').decode()}}
        self.mesh={'requestID':'nonce','requestSecretUID':'current-secret','receipt':{'id':'nonce','secretUID':'current-secret','hash':digest,'appliedAt':self.now.isoformat()}}

    def test_exact_live_delivery(self):
        self.assertTrue(delivered(self.secret,self.mesh,self.now))

    def test_stale_or_future_receipts_do_not_prove_delivery(self):
        for seconds in [-31, 1]:
            self.mesh['receipt']['appliedAt']=(self.now+datetime.timedelta(seconds=seconds)).isoformat()
            self.assertFalse(delivered(self.secret,self.mesh,self.now))

    def test_old_request_or_secret_cannot_acknowledge_current_payload(self):
        for field in ['id','secretUID','hash']:
            mesh=copy.deepcopy(self.mesh);mesh['receipt'][field]='obsolete'
            self.assertFalse(delivered(self.secret,mesh,self.now))

    def test_native_receipt_without_cluster_ack_is_incomplete(self):
        self.secret['metadata']['annotations']={}
        self.assertFalse(delivered(self.secret,self.mesh,self.now))

    def test_missing_receipt_is_incomplete(self):
        self.mesh['receipt']=None
        self.assertFalse(delivered(self.secret,self.mesh,self.now))

if __name__=='__main__': unittest.main()
