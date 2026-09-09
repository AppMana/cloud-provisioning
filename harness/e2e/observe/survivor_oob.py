"""Out-of-band CRI executor for survivor probes in existing VM containers.

The mapping is trusted local configuration: each Node UID names an OS and an
argv prefix ending in that VM's crictl executable and runtime endpoint flags.
QEMU uses its serial guest agent; EC2 uses the existing awsnode/SSM executor.
Pod identity discovery remains read-only Kubernetes API traffic.
"""
import re
import subprocess

from pod_matrix import Kubectl, CURL


class OutOfBand(Kubectl):
    def __init__(self, server, bastion, namespace, executors):
        super().__init__(server, bastion, namespace)
        if not isinstance(executors, dict) or not executors:
            raise ValueError('nonempty Node UID executor mapping required')
        for uid, spec in executors.items():
            if (not isinstance(uid, str) or not uid or not isinstance(spec, dict) or spec.get('os') not in CURL or spec.get('runtime', 'cri') != 'cri' or
                    not isinstance(spec.get('command'), list) or not spec['command'] or
                    any(not isinstance(a, str) or not a or '\x00' in a for a in spec['command'])):
                raise ValueError('each executor requires OS and nonempty argv command')
        self.executors = executors

    def exec_result(self, source, arguments, timeout=30):
        """Execute argv in the exact ordinary container through its VM runtime."""
        if (not isinstance(arguments, list) or not arguments or
                any(not isinstance(a, str) or not a or '\x00' in a for a in arguments)):
            raise ValueError('nonempty container argv required')
        spec = self.executors.get(source['nodeUID'])
        if spec is None or spec['os'] != source['os']:
            raise ValueError('missing executor or OS mismatch for exact Node UID')
        match = re.fullmatch(r'containerd://([0-9a-f]{64})', source['containerID'])
        if not match:
            raise ValueError('a full containerd container identity is required')
        command = spec['command'] + ['exec', match[1]] + arguments
        try:
            result = subprocess.run(command, capture_output=True, timeout=timeout+45)
            return result.stdout, result.returncode, result.stderr.decode('utf-8', errors='replace')
        except subprocess.TimeoutExpired:
            return b'', 124, 'Out-of-band probe observation timed out; inspect original executor before retrying lifecycle actions'

    def curl_result(self, source, arguments, body=None, timeout=30):
        if body is not None:
            raise ValueError('out-of-band survivor probes do not stage stdin')
        return self.exec_result(source, [CURL[source['os']],
            '--silent', '--show-error', '--fail', '--max-time', str(timeout),
            '--noproxy', '*'] + arguments, timeout)
