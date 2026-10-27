"""Characterize source-preserving handover on the native Linux WireGuard path.

Run explicitly inside fresh mount AND network namespaces of an isolated lab VM:
  unshare --mount --net --propagation private python3 wireguard_handover_native.py --mode dual
The single-device control expects loss during mismatched ownership. The dual
receiver case requires delivery in both asymmetric directions. Neither case
qualifies a product deployment. Requires iproute2, sysctl, mount and wg.
"""
import argparse
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import time

def require_isolation():
    for kind in ('net', 'mnt'):
        if os.readlink('/proc/self/ns/' + kind) == os.readlink('/proc/1/ns/' + kind):
            raise RuntimeError('refusing the host ' + kind + ' namespace')
    links = json.loads(subprocess.check_output(['ip', '-j', 'link']))
    if any((link['ifname'] != 'lo' for link in links)):
        raise RuntimeError('network namespace must be empty')
    if shutil.which('wg') is None:
        raise RuntimeError('WireGuard CLI must be staged before the experiment')

def outcomes_expected(phases, mode):
    names = {'both-directions-old', 'old-outbound-new-return',
             'both-directions-new', 'new-outbound-old-return'}
    if mode not in ('single', 'dual') or set(phases) != names:
        return False
    for name, rows in phases.items():
        expected = mode == 'dual' or name in ('both-directions-old', 'both-directions-new')
        if len(rows) != 3:
            return False
        for row in rows:
            if row.get('ok') is not expected:
                return False
            if expected:
                if (row.get('receiverObservedSource') != '10.90.0.10' or
                        row.get('replySource') != '10.88.0.3'):
                    return False
            elif row.get('error') != 'timeout':
                return False
    return True


def experiment(mode, planner=None, runtime=None, work_dir=None):
    if runtime and (mode != 'dual' or planner):
        raise ValueError('runtime requires dual mode without a separate planner')
    if planner and mode != 'dual':
        raise ValueError('planner requires independent receiver mode')
    if mode not in ('single', 'dual'):
        raise ValueError('unsupported receiver mode')
    require_isolation()
    if not work_dir:
        raise ValueError('a private work directory is required for the receivers\' keys')

    def run(*args, input=None):
        return subprocess.check_output(args, input=input, stderr=subprocess.PIPE)

    def ns(name, *args, input=None):
        return run('ip', 'netns', 'exec', name, *args, input=input)
    run('mount', '--make-rprivate', '/')
    run('mount', '-t', 'tmpfs', 'tmpfs', '/run')
    pathlib.Path('/run/netns').mkdir()
    run('ip', 'link', 'set', 'lo', 'up')
    for bridge in ['underlay', 'site']:
        run('ip', 'link', 'add', bridge, 'type', 'bridge')
        run('ip', 'link', 'set', bridge, 'up')
    names = ['old', 'new', 'cloud', 'pod']
    procs = []

    def attach(name, iface, bridge, address):
        peer = name + iface
        run('ip', 'link', 'add', peer, 'type', 'veth', 'peer', 'name', iface, 'netns', name)
        run('ip', 'link', 'set', peer, 'master', bridge)
        run('ip', 'link', 'set', peer, 'up')
        ns(name, 'ip', 'address', 'add', address, 'dev', iface)
        ns(name, 'ip', 'link', 'set', iface, 'up')
    try:
        with tempfile.TemporaryDirectory(prefix='dual-receiver-', dir=work_dir) as directory:
            os.chmod(directory, 448)
            paths = {}
            pubs = {}
            for name in names:
                run('ip', 'netns', 'add', name)
                ns(name, 'ip', 'link', 'set', 'lo', 'up')
                for key, value in [('net.ipv4.conf.all.rp_filter', '0'), ('net.ipv4.conf.default.rp_filter', '0'), ('net.ipv4.ip_forward', '1')]:
                    ns(name, 'sysctl', '-q', '-w', key + '=' + value)
            for i, name in enumerate(['old', 'new', 'cloud'], 1):
                attach(name, 'outer', 'underlay', f'198.18.0.{i}/24')
            for i, name in [(1, 'old'), (2, 'new'), (10, 'pod')]:
                attach(name, 'inner', 'site', f'10.90.0.{i}/24')
            for name in ['old', 'new', 'receiver-old', 'receiver-new', 'receiver-root']:
                key = run('wg', 'genkey')
                pubs[name] = run('wg', 'pubkey', input=key).decode().strip()
                path = pathlib.Path(directory) / name
                path.write_bytes(key)
                path.chmod(384)
                paths[name] = str(path)
            receivers = {name: dict(interface='wg'+name, allowedSources=['10.90.0.10/32']) for name in ['old', 'new']}
            if planner:
                request = dict(meshID='native-receiver-contract',
                    previous=dict(peers=[dict(publicKey=pubs['old'], allowedIPs=['10.90.0.10/32'])]),
                    desired=dict(peers=[dict(publicKey=pubs['new'], allowedIPs=['10.90.0.10/32'], routeHosts=['10.90.0.10'])]),
                    native=['172.29.0.0/24'])
                plan = json.loads(run(planner, input=json.dumps(request).encode()))
                if plan['desired'] != request['desired']:
                    raise RuntimeError('receiver preparation changed the desired egress document')
                by_key = {path['publicKey']: path for path in plan['receivers']}
                if set(by_key) != {pubs['old'], pubs['new']}:
                    raise RuntimeError('receiver plan omitted or added an endpoint')
                receivers = {name: by_key[pubs[name]] for name in ['old', 'new']}
            bindings = None
            if runtime:
                request = dict(meshID='native-receiver-contract',
                    previous=dict(peers=[dict(publicKey=pubs['old'], allowedIPs=['10.90.0.10/32'])]),
                    desired=dict(peers=[dict(publicKey=pubs['new'], allowedIPs=['10.90.0.10/32'], routeHosts=['10.90.0.10'])]),
                    native=['172.29.0.0/24'], rootKeyFile=paths['receiver-root'],
                    resultFile=str(pathlib.Path(directory) / 'bindings.json'))
                request_path = pathlib.Path(directory) / 'request.json'
                request_path.write_text(json.dumps(request))
                try:
                    ns('cloud', 'env', 'CLDT_NATIVE_RECEIVE_EXCHANGE=' + str(request_path),
                       runtime, '-test.run', '^TestNativeReceiveExchange$', '-test.v')
                except subprocess.CalledProcessError as error:
                    print(error.output.decode(errors='replace'), flush=True)
                    raise
                rows = json.loads(pathlib.Path(request['resultFile']).read_text())
                bindings = {row['peerPublicKey']: row for row in rows}
                if len(rows) != 2 or set(bindings) != {pubs['old'], pubs['new']}:
                    raise RuntimeError('runtime omitted or added a receiver')
                receivers = {name: dict(interface=bindings[pubs[name]]['interface'],
                                        allowedSources=['10.90.0.10/32']) for name in ['old', 'new']}
            devices = [('old', 'wgtest', 'old', 51820), ('new', 'wgtest', 'new', 51820), ('cloud', receivers['old']['interface'], 'receiver-old', 51820)]
            if mode == 'dual':
                devices.append(('cloud', receivers['new']['interface'], 'receiver-new', 51821))
            if runtime:
                devices = [device for device in devices if device[0] != 'cloud']
            for name, iface, key, port in devices:
                ns(name, 'ip', 'link', 'add', iface, 'type', 'wireguard')
                ns(name, 'wg', 'set', iface, 'private-key', paths[key], 'listen-port', str(port))
                ns(name, 'ip', 'link', 'set', iface, 'up')
            ns('cloud', 'ip', 'address', 'add', '10.88.0.3/32', 'dev', 'lo')
            for i, name in enumerate(['old', 'new'], 1):
                receiver_public = bindings[pubs[name]]['publicKey'] if runtime else pubs['receiver-' + (name if mode == 'dual' else 'old')]
                receiver_port = bindings[pubs[name]]['listenPort'] if runtime else (51819 + i if mode == 'dual' else 51820)
                ns(name, 'wg', 'set', 'wgtest', 'peer', receiver_public, 'endpoint', '198.18.0.3:' + str(receiver_port), 'allowed-ips', '10.88.0.3/32', 'persistent-keepalive', '1')
                ns(name, 'ip', 'route', 'add', '10.88.0.3/32', 'dev', 'wgtest')
                if not runtime:
                    ns('cloud', 'wg', 'set', receivers[name if mode == 'dual' else 'old']['interface'], 'peer', pubs[name], 'endpoint', f'198.18.0.{i}:51820', 'allowed-ips', ','.join(receivers[name]['allowedSources']) if mode == 'dual' or name == 'old' else '10.90.0.99/32', 'persistent-keepalive', '1')
            ns('pod', 'ip', 'route', 'add', '10.88.0.3/32', 'via', '10.90.0.1')
            ns('cloud', 'ip', 'route', 'add', '10.90.0.10/32', 'dev', receivers['old']['interface'])
            server = "import socket,json;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('10.88.0.3',8081));\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(json.dumps(dict(body=b.decode(),source=a[0])).encode(),a)"
            procs.append(subprocess.Popen(['ip', 'netns', 'exec', 'cloud', 'python3', '-c', server], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE))
            time.sleep(2)
            sender = "import socket,json,time\nrows=[]\nfor i in range(3):\n s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('10.90.0.10',0));s.settimeout(1);body='probe-'+str(i);s.sendto(body.encode(),('10.88.0.3',8081))\n try:\n  b,a=s.recvfrom(4096);r=json.loads(b);rows.append(dict(ok=r['body']==body and r['source']=='10.90.0.10',receiverObservedSource=r['source'],replySource=a[0]))\n except TimeoutError:rows.append(dict(ok=False,error='timeout'))\n finally:s.close()\n time.sleep(.1)\nprint(json.dumps(rows))"
            result = dict(scope='Isolated kernel handover characterization', mode=mode, qualificationEligible=False, isolatedFromGuestHost=True, plannerUsed=bool(planner or runtime), runtimeUsed=bool(runtime), phases={})
            result['phases']['both-directions-old'] = json.loads(ns('pod', 'python3', '-c', sender))
            if mode == 'dual':
                ns('cloud', 'ip', 'route', 'replace', '10.90.0.10/32', 'dev', receivers['new']['interface'])
            else:
                ns('cloud', 'wg', 'set', 'wgold', 'peer', pubs['new'], 'allowed-ips', '10.90.0.10/32')
            result['phases']['old-outbound-new-return'] = json.loads(ns('pod', 'python3', '-c', sender))
            ns('pod', 'ip', 'route', 'replace', '10.88.0.3/32', 'via', '10.90.0.2')
            result['phases']['both-directions-new'] = json.loads(ns('pod', 'python3', '-c', sender))
            if mode == 'dual':
                ns('cloud', 'ip', 'route', 'replace', '10.90.0.10/32', 'dev', receivers['old']['interface'])
            else:
                ns('cloud', 'wg', 'set', 'wgold', 'peer', pubs['old'], 'allowed-ips', '10.90.0.10/32')
            result['phases']['new-outbound-old-return'] = json.loads(ns('pod', 'python3', '-c', sender))
            result['expectedOutcomesObserved'] = outcomes_expected(result['phases'], mode)
            print(json.dumps(result), flush=True)
            if not result['expectedOutcomesObserved']:
                raise RuntimeError('Native observations differ from the handover contract')
    finally:
        for proc in procs:
            proc.terminate()
            proc.wait()
        for name in reversed(names):
            subprocess.run(['ip', 'netns', 'delete', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['single', 'dual'], required=True)
    parser.add_argument('--planner', help='staged receiveplan executable; consumes the public request on stdin')
    parser.add_argument('--runtime', help='staged dialer test binary; prepares real receivers in the cloud namespace')
    parser.add_argument('--work-dir', required=True, help='private directory for the receivers\' keys')
    args = parser.parse_args()
    experiment(args.mode, args.planner, args.runtime, args.work_dir)
