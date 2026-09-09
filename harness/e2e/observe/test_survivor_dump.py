import base64
import hashlib
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

from survivor_dump import dump_linux


class DumpTest(unittest.TestCase):
    source = {'nodeUID': 'node-uid', 'os': 'linux', 'containerID': 'containerd://'+'a'*64}
    mapping = {'node-uid': {'os': 'linux', 'hostCommand': ['guest'], 'command': ['guest', 'crictl', '--runtime-endpoint=socket']}}

    def response(self, payload, offset, chunk=12000):
        return dict(offset=offset, total=len(payload), sha256=hashlib.sha256(payload).hexdigest(),
                    data=base64.b64encode(payload[offset:offset+chunk]).decode())

    def test_real_host_transports_binary_larger_than_ssm_inline_limit(self):
        payload = bytes(range(256))*150
        with tempfile.TemporaryDirectory() as directory:
            script = pathlib.Path(directory)/'container.py'
            script.write_text('import sys;assert sys.argv[1:3]==["exec","'+'a'*64+'"];sys.stdout.buffer.write(bytes(range(256))*150)')
            mapping = {'node-uid': {'os': 'linux', 'hostCommand': ['env'], 'command': ['env', sys.executable, str(script)]}}
            output = dump_linux(mapping, self.source, ['sampler', '-dump'])
            self.assertEqual(output, payload)

    def test_partial_changed_and_corrupt_evidence_is_rejected(self):
        payload = b'x'*12001
        for failure in ['process', 'limit', 'offset', 'size', 'digest', 'short', 'corrupt', 'changed', 'base64']:
            with self.subTest(failure=failure):
                count = 0
                def run(argv, **kwargs):
                    nonlocal count
                    offset = int(argv[4]); row = self.response(payload, offset); count += 1
                    if failure == 'process': return subprocess.CompletedProcess(argv, 1, b'', b'private diagnostic')
                    if failure == 'limit': return subprocess.CompletedProcess(argv, 0, b'x'*24000)
                    if failure == 'offset': row['offset'] = True
                    if failure == 'size': row['total'] = 64*1024*1024+1
                    if failure == 'digest': row['sha256'] = 'invalid'
                    if failure == 'short': row['data'] = ''
                    if failure == 'corrupt': row['data'] = base64.b64encode(b'y'*min(12000,len(payload)-offset)).decode()
                    if failure == 'changed' and count == 2: row['sha256'] = '0'*64
                    if failure == 'base64': row['data'] = '!'
                    return subprocess.CompletedProcess(argv, 0, json.dumps(row).encode())
                with self.assertRaises((ValueError, RuntimeError)):
                    dump_linux(self.mapping, self.source, ['sampler', '-dump'], run=run)

    def test_requires_exact_supported_executor_and_bounded_sizes(self):
        for source, mapping, kwargs in [
                ({**self.source, 'os': 'windows'}, self.mapping, {}),
                ({**self.source, 'containerID': 'containerd://short'}, self.mapping, {}),
                (self.source, {}, {}),
                (self.source, self.mapping, {'chunk_size': 12001}),
                (self.source, self.mapping, {'max_bytes': 0})]:
            with self.assertRaises(ValueError):
                dump_linux(mapping, source, ['sampler', '-dump'], **kwargs)


if __name__ == '__main__':
    unittest.main()
