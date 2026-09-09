"""A green aggregate must not hide omitted paths or missing machine evidence."""
import json
from pathlib import Path
import tempfile
import unittest

from results import row


class EvidenceTest(unittest.TestCase):
    def test_duplicate_path_cannot_replace_missing_transfer(self):
        # Node identity and 102-path shape observed in the real AWS survivor row.
        nodes = ['cp', 'cp2', 'cp3', 'w1', 'w2', 'ip-172-29-0-105']
        checks = [{'from': a, 'to': b, 'kind': kind, 'passed': True}
                  for a in nodes for b in nodes if a != b
                  for kind in ('pod', 'service', 'transfer')]
        checks += [{'from': a, 'to': '', 'kind': kind, 'passed': True}
                   for a in nodes for kind in ('dns', 'external')]
        binding = {'machine': 'aws-k0s-2', 'node': nodes[-1],
                   'instanceID': 'i-035c493d9939663cb',
                   'providerID': 'aws:///us-west-2a/i-035c493d9939663cb',
                   'nodeUID': 'bd36dcd0-8d27-44f9-a330-ae7a70b8ff41',
                   'physicalNIC': 'ens5', 'eniCount': 1}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / 'bindings.json').write_text(json.dumps([binding]))
            def write():
                (path / 'matrix.json').write_text(json.dumps({
                    'total': 102, 'passed': 102, 'failed': 0, 'results': checks}))
            write()
            self.assertEqual(row(path, 102)['passed'], 102)
            checks[2] = checks[0].copy()
            write()
            with self.assertRaisesRegex(ValueError, 'network paths'):
                row(path, 102)


if __name__ == '__main__':
    unittest.main()
