import subprocess
import unittest
from unittest.mock import patch

from survivor_oob import OutOfBand


class OOBTest(unittest.TestCase):
    def test_both_guest_types_pin_exact_container_and_preserve_arguments(self):
        for os_name,curl in [('linux','curl'),('windows',r'C:\curl\curl.exe')]:
            with self.subTest(os=os_name):
                kube=OutOfBand('https://test','test','test',{'uid':dict(os=os_name,command=['executor','crictl'])})
                source=dict(nodeUID='uid',os=os_name,containerID='containerd://'+'a'*64)
                reply=subprocess.CompletedProcess([],0,b'{"responses":[]}',b'diagnostic')
                with patch('survivor_oob.subprocess.run',return_value=reply) as run:
                    out,code,err=kube.curl_result(source,['http://127.0.0.1:8080/dial?request=echo+x&tries=1'])
                argv=run.call_args.args[0]
                self.assertEqual(argv[:5],['executor','crictl','exec','a'*64,curl])
                self.assertEqual(argv[-1],'http://127.0.0.1:8080/dial?request=echo+x&tries=1')
                self.assertEqual((out,code,err),(reply.stdout,0,'diagnostic'))

    def test_unqualified_ctr_executor_rejected(self):
        with self.assertRaises(ValueError):
            OutOfBand('https://test','test','test',{'uid':dict(os='windows',runtime='ctr',command=['k0s.exe','ctr'])})

    def test_wrong_identity_or_os_never_executes(self):
        kube=OutOfBand('https://test','test','test',{'uid':dict(os='linux',command=['executor'])})
        for source in [dict(nodeUID='other',os='linux',containerID='containerd://'+'a'*64),
                       dict(nodeUID='uid',os='windows',containerID='containerd://'+'a'*64),
                       dict(nodeUID='uid',os='linux',containerID='short-id')]:
            with patch('survivor_oob.subprocess.run') as run,self.assertRaises(ValueError):
                kube.curl_result(source,[])
            run.assert_not_called()

    def test_timeout_is_failed_observation(self):
        kube=OutOfBand('https://test','test','test',{'uid':dict(os='linux',command=['executor'])})
        source=dict(nodeUID='uid',os='linux',containerID='containerd://'+'a'*64)
        with patch('survivor_oob.subprocess.run',side_effect=subprocess.TimeoutExpired('executor',55)):
            self.assertEqual(kube.curl_result(source,[])[1],124)

    def test_invalid_executor_mapping_and_stdin_rejected(self):
        for mapping in [{},{'uid':dict(os='windows',command='shell command')},
                        {'uid':dict(os='linux',command=[''])}]:
            with self.assertRaises(ValueError):OutOfBand('https://test','test','test',mapping)
        kube=OutOfBand('https://test','test','test',{'uid':dict(os='linux',command=['executor'])})
        with self.assertRaises(ValueError):kube.curl_result({},[],body=b'private input')
