"""Collect completed Linux sampler dumps without Kubernetes exec or large SSM output.

The Linux VM image must provide Python 3 on the host. Execution uses a trusted
host argv prefix and the exact container identity already captured by the
sampler. Each bounded response binds its bytes to the whole compressed dump.
The existing survivor validator independently checks the decompressed log.
"""
import base64
import hashlib
import json
import re
import subprocess


HOST_READ = """
import base64,hashlib,json,subprocess,sys
offset,size,maximum=map(int,sys.argv[1:4])
r=subprocess.run(sys.argv[4:],capture_output=True,timeout=45)
if r.returncode:raise SystemExit('native container dump failed')
data=r.stdout
if not 0<len(data)<=maximum or not 0<=offset<len(data):raise SystemExit('invalid dump size or offset')
print(json.dumps({'offset':offset,'total':len(data),'sha256':hashlib.sha256(data).hexdigest(),'data':base64.b64encode(data[offset:offset+size]).decode()}))
"""


def dump_linux(executors, source, arguments, *, run=subprocess.run,
               chunk_size=12000, max_bytes=64*1024*1024):
    if source.get('os') != 'linux':
        raise ValueError('Linux host dump adapter required')
    spec = executors.get(source['nodeUID'], {})
    host = spec.get('hostCommand')
    runtime = spec.get('command')
    cid = re.fullmatch(r'containerd://([0-9a-f]{64})', source.get('containerID', ''))
    if (not cid or not isinstance(host, list) or not host or
            not isinstance(runtime, list) or runtime[:len(host)] != host or
            len(runtime) <= len(host) or spec.get('os') != 'linux' or
            any(not isinstance(a, str) or not a or '\0' in a for a in runtime) or
            not isinstance(arguments, list) or not arguments or
            any(not isinstance(a, str) or not a or '\0' in a for a in arguments)):
        raise ValueError('exact Linux host and container executor required')
    if type(chunk_size) is not int or not 1 <= chunk_size <= 12000 or type(max_bytes) is not int or not 1 <= max_bytes <= 64*1024*1024:
        raise ValueError('bounded chunk and total sizes required')
    data = bytearray()
    total = digest = None
    while total is None or len(data) < total:
        argv = host + ['python3', '-c', HOST_READ, str(len(data)),
                       str(chunk_size), str(max_bytes)] + runtime[len(host):] + ['exec', cid[1]] + arguments
        result = run(argv, capture_output=True, timeout=120)
        if result.returncode:
            raise RuntimeError('native dump observation failed; retain original sampler files')
        if len(result.stdout) >= 24000:
            raise ValueError('native dump response reached the SSM inline limit')
        row = json.loads(result.stdout)
        if (type(row.get('offset')) is not int or row['offset'] != len(data) or
                type(row.get('total')) is not int or not 0 < row['total'] <= max_bytes or
                not isinstance(row.get('sha256'), str) or not re.fullmatch('[0-9a-f]{64}', row['sha256']) or
                not isinstance(row.get('data'), str)):
            raise ValueError('invalid native dump chunk metadata')
        if total is None:
            total, digest = row['total'], row['sha256']
        if row['total'] != total or row['sha256'] != digest:
            raise ValueError('native dump changed during collection')
        chunk = base64.b64decode(row['data'], validate=True)
        if len(chunk) != min(chunk_size, total-len(data)):
            raise ValueError('missing or truncated native dump chunk')
        data.extend(chunk)
    if hashlib.sha256(data).hexdigest() != digest:
        raise ValueError('native dump hash mismatch')
    return bytes(data)
