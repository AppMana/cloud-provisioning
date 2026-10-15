import unittest
import pathlib
import subprocess
import sys
import tempfile
from vpc_cni import values


class NativeCNITest(unittest.TestCase):
    def render(self, **changes):
        options = dict(family='ipv4', site_cidrs=['10.101.0.0/24', '10.101.128.0/17', 'fd8f:cf26:522a:128::/64'],
                       service_cidrs=['10.101.4.0/22', 'fd8f:cf26:522a:4::/108'],
                       vpc_cidrs=['172.29.0.0/24', '2001:db8:100::/64'], run_id='cldt-native',
                       image='cni@sha256:' + 'a'*64, init_image='init@sha256:' + 'b'*64)
        options.update(changes)
        return values(**options)

    def test_families_set_both_initialization_and_ipam_without_dualstack(self):
        for family in ('ipv4', 'ipv6'):
            result = self.render(family=family)
            v6 = str(family == 'ipv6').lower()
            self.assertEqual(result['env']['ENABLE_IPv6'], v6)
            self.assertEqual(result['init']['env']['ENABLE_IPv6'], v6)
            self.assertNotEqual(result['env']['ENABLE_IPv4'], v6)
            self.assertEqual(result['env']['ENABLE_PREFIX_DELEGATION'], v6)
            self.assertEqual(result['nodeSelector']['kubernetes.io/os'], 'linux')
            self.assertEqual(result['nodeSelector']['cloud-provisioning.appmana.com/cni'], 'aws-vpc')
            self.assertFalse(result['nodeAgent']['enabled'])
            self.assertEqual(result['env']['AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS'], '10.101.0.0/24,10.101.128.0/17,10.101.4.0/22')

    def test_wrong_chart_cannot_emit_manifests(self):
        with tempfile.TemporaryDirectory() as directory:
            chart = pathlib.Path(directory) / 'chart.tgz'
            chart.write_bytes(b'untrusted chart')
            result = subprocess.run([sys.executable, str(pathlib.Path(__file__).with_name('vpc_cni.py')),
                '--chart', str(chart), '--family', 'ipv4', '--site-cidr', '10.101.128.0/17',
                '--service-cidr', '10.101.4.0/22', '--vpc-cidr', '172.29.0.0/24',
                '--run-id', 'cldt-invalid', '--image', 'cni@sha256:' + 'a'*64,
                '--init-image', 'init@sha256:' + 'b'*64], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, '')
            self.assertIn('chart archive digest', result.stderr)

    def test_invalid_campaign_cannot_render_installable_values(self):
        for changes in (dict(family='dual'), dict(vpc_cidrs=['10.101.0.0/16']),
                        dict(site_cidrs=['0.0.0.0/0']), dict(service_cidrs=[]),
                        dict(family='ipv6', vpc_cidrs=['172.29.0.0/24']),
                        dict(mtu=1500), dict(mtu=1200), dict(image='cni:latest'),
                        dict(run_id='production'), dict(site_cidrs=['10.0.0.1/24'])):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.render(**changes)


if __name__ == '__main__':
    unittest.main()
