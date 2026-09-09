import copy
import json
import pathlib
import unittest
from linux_image_reuse import evaluate


class NativeImageReuse(unittest.TestCase):
    def setUp(self):
        self.native = json.loads((pathlib.Path(__file__).parent/'testdata/linux-k0s-image-reuse.json').read_text())

    def test_actual_fresh_capa_worker_reused_image_executable(self):
        result = evaluate(**self.native)
        self.assertTrue(result['passed'])
        self.assertEqual(result['containerdVersions'],{'Client':'2.3.2','Server':'2.3.2'})

    def test_wrong_image_rewritten_binary_changed_receipt_or_management_nic_fails(self):
        cases = [('binding','imageID','ami-other'),('binding','eniCount',2),
                 ('observation','mtime',9999999999),('observation','mtime',float('nan')),
                 ('observation','sha256','0'*64),('observation','receipt',{}),
                 ('observation','physicalNICs',['ens5','ens6'])]
        for section, field, value in cases:
            with self.subTest(field=field):
                native = copy.deepcopy(self.native);native[section][field] = value
                self.assertFalse(evaluate(**native)['passed'])

    def test_containerd_two_client_cannot_hide_server_one(self):
        self.native['observation']['containerd'] = self.native['observation']['containerd'].replace('Server:\n  Version:  2.3.2','Server:\n  Version:  1.7.28')
        self.assertFalse(evaluate(**self.native)['checks']['containerdServer2'])

    def test_composed_gpu_image_requires_child_ami_even_with_identical_k0s_layer(self):
        native = json.loads((pathlib.Path(__file__).parent/'testdata/linux-k0s-gpu-image-reuse.json').read_text())
        self.assertTrue(evaluate(**native)['passed'])
        # The CPU parent has the same k0s executable but lacks the GPU layer.
        native['binding']['imageID'] = native['candidate']['parentImageID']
        result = evaluate(**native)
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['candidateImageMatches'])
        self.assertTrue(result['checks']['binaryHashMatches'])


if __name__ == '__main__': unittest.main()
