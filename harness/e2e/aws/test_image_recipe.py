import hashlib
import pathlib
import tempfile
import unittest
import image_recipe


class ImageRecipeTest(unittest.TestCase):
    def test_worker_only_revision_is_a_change(self):
        record = dict(tunnelSHA256='tunnel', workerSHA256='old', workerVersion='v1')
        self.assertFalse(image_recipe.same_recipe(record, 'tunnel', dict(workerSHA256='new', workerVersion='v2')))
        self.assertTrue(image_recipe.same_recipe(record, 'tunnel', dict(workerSHA256='old', workerVersion='v1')))
        self.assertFalse(image_recipe.same_recipe(record, 'changed-tunnel', {}))

    def test_requires_a_complete_checksum_verified_identity(self):
        self.assertEqual(image_recipe.worker_artifact(None, None, None), {})
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'worker.exe'
            path.write_bytes(b'artifact')
            sha = hashlib.sha256(path.read_bytes()).hexdigest()
            version = 'v1.36.2+k0s.0.appmana.1'
            self.assertEqual(image_recipe.worker_artifact(path, sha.upper(), version),
                             dict(workerSHA256=sha, workerVersion=version))
            for args in [(path, None, version), (path, '0'*64, version),
                         (path, sha, "v1.36.2+k0s.0';bad"), (path, 'bad', version),
                         (path.with_name('absent'), sha, version)]:
                with self.assertRaises(ValueError):
                    image_recipe.worker_artifact(*args)


class GPUReceipt(unittest.TestCase):
    def test_native_receipt_and_rejection_of_wrong_os_or_driver(self):
        from image_recipe import gpu_evidence
        evidence={'windowsBuild':26100,'verifiedAfterReboot':True,
                  'licenseScope':'aws','licenseSource':'https://docs.aws.amazon.com/',
                  'bootTime':'2026-09-07T16:28:56.5000000Z','packageSHA256':'b'*64,
                  'driverVersion':'582.53','gpuCount':1,
                  'gpus':[{'driverVersion':'582.53','driverModel':'WDDM','name':'NVIDIA L4-3Q'}]}
        self.assertEqual(gpu_evidence(evidence,'2025'),evidence)
        for change in [{'windowsBuild':20348},{'verifiedAfterReboot':False},
                       {'licenseScope':'gce'},{'gpuCount':0},{'gpus':[]},
                       {'driverVersion':'596.86'}, {'packageSHA256':'invalid'},
                       {'gpus':[{'driverVersion':'582.53','driverModel':'TCC'}]}]:
            with self.subTest(change=change),self.assertRaises(ValueError):
                gpu_evidence(dict(evidence,**change),'2025')

    def test_gpu_layer_rejects_accidental_tunnel_downgrade(self):
        from image_recipe import gpu_base_identity
        base={'imageID':'ami-cpu','tunnelSHA256':'current'}
        builder={'baseImageID':'ami-cpu'}
        gpu_base_identity(builder,base,'current')
        for actual_builder,actual_base,sha,worker in [
                (builder,base,'old',None), (builder,base,'current',{'workerVersion':'new'}),
                ({'baseImageID':'ami-other'},base,'current',None),
                (builder,{},'current',None)]:
            with self.assertRaises(ValueError):
                gpu_base_identity(actual_builder,actual_base,sha,worker)


if __name__ == '__main__':
    unittest.main()
