import copy
import unittest
import tempfile
import pathlib
import json
import hashlib
import subprocess
from unittest.mock import patch
from renew_artifact import main
from renew_artifact import artifact,url_patch


class RenewalTest(unittest.TestCase):
    def setUp(self):
        self.state=dict(assetBucket='owned',region='us-west-2')
        self.deployment={'metadata':{'resourceVersion':'42'},'spec':{'template':{'spec':{'containers':[{'args':[
            '--join-dialer-binary-url-amd64=https://owned.s3.us-west-2.amazonaws.com/downloads/dialer?X-Amz-Date=20260101T000000Z&X-Amz-Expires=60',
            '--join-dialer-binary-sha256-amd64='+'a'*64]}]}}}}

    def test_expired_url_selects_same_object_and_conditional_update(self):
        selected=artifact(self.deployment,self.state,'amd64')
        self.assertEqual(selected['key'],'downloads/dialer')
        patch=url_patch(self.deployment,selected,'https://new-private-url')
        self.assertEqual(patch[0],dict(op='test',path='/metadata/resourceVersion',value='42'))
        self.assertEqual(patch[1]['path'],'/spec/template/spec/containers/0/args/0')

    def test_foreign_object_and_missing_checksum_rejected(self):
        for replacement in ['https://foreign.s3.us-west-2.amazonaws.com/downloads/dialer',
                            'https://owned.s3.us-west-2.amazonaws.com/private/file',
                            'http://owned.s3.us-west-2.amazonaws.com/downloads/dialer']:
            d=copy.deepcopy(self.deployment);d['spec']['template']['spec']['containers'][0]['args'][0]='--join-dialer-binary-url-amd64='+replacement
            with self.assertRaises(ValueError):artifact(d,self.state,'amd64')
        self.deployment['spec']['template']['spec']['containers'][0]['args'].pop()
        with self.assertRaises(ValueError):artifact(self.deployment,self.state,'amd64')

    def test_ambiguous_or_wrong_architecture_never_selects_arbitrarily(self):
        with self.assertRaises(ValueError):artifact(self.deployment,self.state,'arm64')
        self.deployment['spec']['template']['spec']['containers']*=2
        with self.assertRaises(ValueError):artifact(self.deployment,self.state,'amd64')

    def test_digest_validation_precedes_signing_and_mutation(self):
        for mismatch in [False,True]:
            with self.subTest(mismatch=mismatch), tempfile.TemporaryDirectory() as root:
                work=pathlib.Path(root); evidence=work/'evidence'
                state=dict(self.state,account='123',cleanedUp=False)
                (work/'resources.json').write_text(json.dumps(state))
                deployment=copy.deepcopy(self.deployment)
                deployment['spec']['template']['spec']['containers'][0]['args'][1]='--join-dialer-binary-sha256-amd64='+hashlib.sha256(b'pinned').hexdigest()
                signed=[];mutations=[]
                def execute(argv,**kw):
                    out='{}'
                    if argv[0]=='aws':
                        if 'get-caller-identity' in argv:out=json.dumps({'Account':'123'})
                        elif 'get-object' in argv:pathlib.Path(argv[-1]).write_bytes(b'changed' if mismatch else b'pinned')
                        elif 'get-federation-token' in argv:
                            self.assertEqual(argv[argv.index('--name')+1],'cldt-assets')
                            policy=json.loads(argv[argv.index('--policy')+1])
                            self.assertEqual(policy['Statement'][0]['Resource'],'arn:aws:s3:::owned/downloads/dialer')
                            signed.append(True)
                            out=json.dumps({'Credentials':{'AccessKeyId':'test','SecretAccessKey':'test','SessionToken':'test','Expiration':'later'}})
                        elif 'presign' in argv:out='https://owned.s3.us-west-2.amazonaws.com/downloads/dialer?fresh'
                        else:self.fail('unexpected AWS operation')
                    elif 'get' in argv:out=json.dumps(deployment)
                    elif 'patch' in argv:mutations.append(json.loads(kw['input']))
                    else:self.fail('unexpected command')
                    return subprocess.CompletedProcess(argv,0,out,'')
                with patch('sys.argv',['renew','--work-dir',str(work),'--evidence-dir',str(evidence),'--api-server','https://test']), patch('renew_artifact.subprocess.run',side_effect=execute), patch('builtins.print'):
                    if mismatch:
                        with self.assertRaises(ValueError):main()
                    else:main()
                self.assertEqual(len(signed),0 if mismatch else 1)
                self.assertEqual(len(mutations),0 if mismatch else 1)
                if not mismatch:self.assertEqual(mutations[0][0]['value'],'42')
