"""Lifecycle guard regressions based on the Windows 2025 native trial."""
import copy
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch
from windows_lifecycle import probe_objects, Trial, validate_config


class ConfigTest(unittest.TestCase):
    def test_native_executors_are_required_before_provisioning(self):
        config=dict(bastion='b',apiServer='https://a',name='n',probeNamespace='ns',sourcePod='s',windowsVersion='2025',
                    workDirectory='w',siteDirectory='site',awsnode='an',awsremove='ar',gateway='g',
                    observerDirectory='o',survivorTargets=['a=x','b=y'])
        with self.assertRaisesRegex(ValueError,'executor'):validate_config(config)
        validate_config(dict(config,observerExecutors='executors.json'))


class RecipeTest(unittest.TestCase):
    def test_native_windows_builds_use_same_recipe_without_old_sa_mount(self):
        source={'spec':{'containers':[{'image':'example@sha256:'+'a'*64,'volumeMounts':[{'name':'old-token'}]}]}}
        old=copy.deepcopy(source);service={'spec':{'ports':[{'port':8080}]}}
        for version,build in [('2022','20348'),('2025','26100')]:
            node={'metadata':{'name':'current','labels':{'kubernetes.io/os':'windows','node.kubernetes.io/windows-build':'10.0.'+build}}}
            objects=probe_objects(source,service,node,'probe','ns',version)
            self.assertNotIn('volumeMounts',objects[0]['spec']['containers'][0])
            self.assertEqual(objects[0]['spec']['nodeSelector']['kubernetes.io/hostname'],'current')
            self.assertEqual(source,old)
            with self.assertRaises(ValueError):probe_objects(source,service,node,'probe','ns','2025' if version=='2022' else '2022')

    def test_unpinned_recipe_rejected_before_launch(self):
        node={'metadata':{'name':'current','labels':{'kubernetes.io/os':'windows','node.kubernetes.io/windows-build':'10.0.26100'}}}
        with self.assertRaises(ValueError):probe_objects({'spec':{'containers':[{'image':'example:latest'}]}},{},node,'probe','ns','2025')


class EvidenceTest(unittest.TestCase):
    def test_failed_original_command_retained_and_output_never_reused(self):
        import sys
        with tempfile.TemporaryDirectory() as root:
            trial=Trial({'bastion':'unused','apiServer':'https://unused'},root)
            with self.assertRaises(RuntimeError):trial.run([sys.executable,'-c','raise SystemExit(7)'],'native')
            self.assertEqual(json.loads((pathlib.Path(root)/'native-exit.json').read_text())['exitCode'],7)
            with self.assertRaises(FileExistsError):trial.run([sys.executable,'-c','pass'],'native')


class SequenceTest(unittest.TestCase):
    def exercise(self, fail_gateway=False, survivor={'ok': True}):
        import types
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        root=pathlib.Path(temp.name);observer=root/'observer';observer.mkdir()
        config=dict(bastion='unused',apiServer='https://unused',name='worker',probeNamespace='ns',
                    observerDirectory=str(observer),survivorTargets=['a=a','b=b'],sourcePod='source',windowsVersion='2025',
                    workDirectory='private',gateway='gateway',
                    awsnode='awsnode',awsremove='awsremove',siteDirectory='site',
                    observerExecutors='unused-executors')
        calls=[]
        class Fake(Trial):
            created=False
            def run(self,args,label):
                calls.append((label,args))
                if label=='create':self.created=True
                if label=='removal':self.save('removal.json',{'terminated':True})
            def get(self,kind,name=None,namespace='cloud-provisioning'):
                if kind=='pod' and name=='source':return {'spec':{'nodeName':'source-node','containers':[{'image':'example@sha256:'+'a'*64}]}}
                if kind=='service' and name=='source':return {'spec':{'ports':[{'port':8080}]}}
                if kind=='node':return {'metadata':{'name':name,'uid':'node-uid','labels':{'kubernetes.io/os':'windows','node.kubernetes.io/windows-build':'10.0.26100'}},'status':{'conditions':[{'type':'Ready','status':'True'}]}}
                if kind=='machine' and self.created:return {'metadata':{'uid':'machine-uid'},'spec':{'providerID':'aws:///zone/i-example'},'status':{'nodeRef':{'name':'worker'}}}
                return None
            def snapshot(self):return {}
        trial=Fake(config,root/'result')
        from unittest.mock import Mock
        native=Mock();native.live_ready.return_value=True;native.finish.return_value=survivor
        trial.native_observer=lambda:native
        def submit(args,**kwargs):
            calls.append(('probe-create',args));return types.SimpleNamespace(returncode=0)
        with patch('windows_lifecycle.preflight',return_value={'passed':not fail_gateway}),patch('windows_lifecycle.evaluate',return_value={'passed':True}),patch('windows_lifecycle.time.sleep'),patch('windows_lifecycle.subprocess.run',side_effect=submit):
            if fail_gateway or not survivor['ok']:
                with self.assertRaises(RuntimeError):trial.execute()
            else:trial.execute()
        return calls,root

    def test_gateway_and_join_gate_precede_single_probe_creation_and_delete(self):
        calls,root=self.exercise();labels=[c[0] for c in calls]
        self.assertEqual(labels,['create','join','probe-create','probe-create','probe-ready','matrix','removal'])
        create=next(args for label,args in calls if label=='create')
        self.assertEqual(create[create.index('--gateway')+1],'gateway')
        self.assertIn('--require-gateway',next(args for label,args in calls if label=='join'))
        operation=json.loads((root/'result/operation.json').read_text())
        self.assertTrue(operation['passed'])
        self.assertTrue(operation['lifecyclePassed'])
        self.assertEqual(operation['survivorStatus'],'complete')

    def test_native_failure_cannot_leave_successful_operation_report(self):
        fixture=json.loads((pathlib.Path(__file__).parent/'testdata/failed-survivor.json').read_text())
        _,root=self.exercise(survivor=fixture)
        operation=json.loads((root/'result/operation.json').read_text())
        self.assertTrue(operation['lifecyclePassed'])
        self.assertFalse(operation['passed'])
        self.assertFalse(operation['survivorPassed'])
        self.assertEqual(json.loads((root/'result/survivor-result.json').read_text()),fixture)

    def test_complete_native_success_is_required_for_combined_pass(self):
        _,root=self.exercise(survivor={'ok':True})
        operation=json.loads((root/'result/operation.json').read_text())
        self.assertTrue(operation['passed'])
        self.assertEqual(operation['survivorStatus'],'complete')
        self.assertFalse(json.loads((root/'result/operation-window.json').read_text())['passed'])

    def test_failed_gateway_preserves_original_binding_without_workload_or_deletion(self):
        calls,root=self.exercise(True)
        self.assertEqual([c[0] for c in calls],['create'])
        self.assertTrue((root/'result/binding.json').exists())
        self.assertFalse((root/'observer/STOP').exists())


if __name__ == '__main__':unittest.main()

class NativeObserverIntegrationTest(unittest.TestCase):
    def test_readiness_uses_native_session(self):
        trial=Trial(dict(bastion='test',apiServer='https://test',survivorTargets=['a=x','b=y']),pathlib.Path('/unused'))
        with patch.object(trial,'native_observer') as observer:
            observer.return_value.live_ready.return_value=True
            self.assertTrue(trial.survivor_ready('before-addition'))
            observer.return_value.live_ready.assert_called_once_with(['a=x','b=y'],'before-addition')

    def test_native_failed_terminal_report_fails_trial(self):
        trial=Trial(dict(bastion='test',apiServer='https://test'),pathlib.Path('/unused'))
        with patch.object(trial,'native_observer') as observer,patch.object(trial,'save') as save:
            observer.return_value.finish.return_value=dict(ok=False)
            with self.assertRaises(RuntimeError):trial.finish_survivors(dict(startedAt='start',finishedAt='end'))
            save.assert_called_once_with('survivor-result.json',dict(ok=False))
