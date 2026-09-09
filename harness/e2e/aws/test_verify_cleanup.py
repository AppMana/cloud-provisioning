import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT=pathlib.Path(__file__).with_name('verify_cleanup.py')

class RepositoryAbsenceTest(unittest.TestCase):
    def exercise(self, remaining=None):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory)
            state=dict(cleanedUp=True,region='us-west-2',account='123',vpcID='vpc-test',runID='cldt-test',nodeRoleName='node',capaRoleName='capa',instanceProfileName='profile',publisherRepository={'name':'cldt-test/windows-publisher'},calicoWindowsRepository={'name':'cldt-test/calico-node-windows'})
            (root/'resources.json').write_text(json.dumps(state));names=[]
            state['windowsGPUProbeRepository']={'name':'cldt-test/windows-gpu-probe'}
            state['windowsGPUPluginRepository']={'name':'cldt-test/windows-gpu-device-plugin'}
            (root/'resources.json').write_text(json.dumps(state))
            def run(args,**kw):
                operation=args[4];inputs=json.loads(args[args.index('--cli-input-json')+1]);result={}
                if operation=='get-caller-identity':result={'Account':'123'}
                elif operation=='describe-instances':result={'Reservations':[]}
                elif operation=='describe-repositories':
                    name=inputs['repositoryNames'][0];names.append(name)
                    if name==remaining:result={'repositories':[{'repositoryName':name}]}
                    else:return subprocess.CompletedProcess(args,1,'','RepositoryNotFoundException')
                return subprocess.CompletedProcess(args,0,json.dumps(result),'')
            with patch.object(sys,'argv',[str(SCRIPT),'--work-dir',str(root)]),patch('subprocess.run',run):
                if remaining:
                    with self.assertRaisesRegex(RuntimeError,'test resource still exists'):
                        runpy.run_path(str(SCRIPT),run_name='__main__')
                    self.assertFalse((root/'cleanup-verification.json').exists())
                else:
                    runpy.run_path(str(SCRIPT),run_name='__main__')
                    self.assertTrue((root/'cleanup-verification.json').exists())
            return names

    def test_all_recorded_repositories_must_be_absent(self):
        self.assertEqual(self.exercise(),['cldt-test/windows-publisher','cldt-test/calico-node-windows','cldt-test/windows-gpu-device-plugin','cldt-test/windows-gpu-probe'])

    def test_calico_repository_cannot_be_ignored(self):
        self.assertIn('cldt-test/calico-node-windows',self.exercise('cldt-test/calico-node-windows'))

    def test_probe_repository_cannot_be_ignored(self):
        self.assertIn('cldt-test/windows-gpu-probe',self.exercise('cldt-test/windows-gpu-probe'))

    def test_gpu_repository_cannot_be_ignored(self):
        self.assertIn('cldt-test/windows-gpu-device-plugin',self.exercise('cldt-test/windows-gpu-device-plugin'))

if __name__=='__main__':unittest.main()
