"""Check an unjoined k0s Linux image cache without pulling or importing images.

Run as root on the builder with linux_cache.py beside this file. The operation
directory must not exist: an interrupted operation must be inspected, not replayed.
"""
import argparse
import hashlib
import json
import os
import pathlib
import shutil
import stat
import subprocess
import time

from linux_cache import evaluate, expected_images

WORKER = pathlib.Path('/usr/local/bin/k0s')
BIN = pathlib.Path('/var/lib/k0s/bin')
ROOT = pathlib.Path('/var/lib/k0s/containerd')
RECEIPTS = pathlib.Path('/etc/cloud-provisioning-image')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def preflight(recipe):
    expected_images(recipe)
    require(os.geteuid() == 0, 'Require root on the unjoined Linux builder')
    for service in ['k0sworker', 'k0scontroller', 'kubelet']:
        state = subprocess.check_output(
            ['systemctl', 'show', service, '--property=LoadState', '--value'], text=True).strip()
        require(state == 'not-found', 'Kubernetes service exists: '+service)
    require(not pathlib.Path('/etc/k0s/k0s.yaml').exists(), 'Cluster configuration exists')
    active = subprocess.run(['pgrep', '-x', 'containerd'], capture_output=True)
    require(active.returncode == 1, 'Require no running containerd before opening the cache')
    require(ROOT.is_dir(), 'Require an existing cache root')
    runtime = json.loads((RECEIPTS/'linux-runtime.json').read_text())
    require(runtime['workerSHA256'] == recipe['workerSHA256'] == sha256(WORKER), 'Worker hash mismatch')
    require(runtime['workerMtimeNs'] == WORKER.stat().st_mtime_ns, 'Worker timestamp changed')
    require({entry['name'] for entry in runtime['components']} ==
            {'containerd', 'containerd-shim-runc-v2', 'runc'}, 'Unexpected runtime components')
    for entry in runtime['components']:
        path = BIN/entry['name']
        require(not path.is_symlink() and sha256(path) == entry['sha256'], 'Runtime bytes changed')
        require(path.stat().st_mtime_ns == runtime['workerMtimeNs'] and
                stat.S_IMODE(path.stat().st_mode) == 0o750, 'Runtime metadata changed')


def observe(recipe, operation):
    preflight(recipe)
    operation.mkdir(mode=0o700)

    def save(name, value):
        with (operation/name).open('x') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')

    save('intent.json', dict(recipeSHA256=hashlib.sha256(
        json.dumps(recipe, sort_keys=True).encode()).hexdigest(), root=str(ROOT)))
    state = pathlib.Path('/run')/('cldt-cache-check-'+str(os.getpid()))
    state.mkdir(mode=0o700)
    socket = str(state/'containerd.sock')
    config = operation/'containerd.toml'
    config.write_text('version = 3\ndisabled_plugins = ["io.containerd.grpc.v1.cri"]\n')
    ctr = [str(WORKER), 'ctr', '--address', socket, '--namespace', 'k8s.io']
    observation = dict(stops=[])
    for phase in ['before', 'after']:
        with (operation/(phase+'.log')).open('xb') as log:
            process = subprocess.Popen(
                [str(BIN/'containerd'), '--root', str(ROOT), '--state', str(state),
                 '--address', socket, '--config', str(config)], stdout=log,
                stderr=subprocess.STDOUT, env=dict(os.environ, PATH=str(BIN)+':'+os.environ['PATH']))
            try:
                save(phase+'-process.json', dict(pid=process.pid))
                for attempt in range(60):
                    require(process.poll() is None, 'Original containerd exited during startup')
                    result = subprocess.run(ctr+['version'], capture_output=True, timeout=5)
                    if result.returncode == 0:
                        break
                    time.sleep(.5)
                else:
                    raise RuntimeError('Original containerd did not become ready')
                for key, args in [(phase, ['images', 'check']),
                                  ('ready'+phase.title(), ['images', 'check', '--quiet'])]:
                    result = subprocess.run(ctr+args, capture_output=True, text=True, timeout=60)
                    (operation/(key+'.stdout')).write_text(result.stdout)
                    (operation/(key+'.stderr')).write_text(result.stderr)
                    require(result.returncode == 0, 'Native cache read failed: '+key)
                    observation[key] = result.stdout
            finally:
                if process.poll() is None:
                    process.terminate()
                code = process.wait(timeout=30)
                stop = dict(exitCode=code, originalProcessExited=True)
                save(phase+'-stopped.json', stop)
                observation['stops'].append(stop)
                require(code == 0, 'Original containerd failed to stop cleanly')
    shutil.rmtree(state)
    observation['temporaryStateRemoved'] = True
    save('observation.json', observation)
    result = evaluate(recipe, observation)
    save('validation.json', result)
    require(result['passed'], 'Native cache gate failed; retain the original operation evidence')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--recipe', type=pathlib.Path, required=True)
    parser.add_argument('--operation', type=pathlib.Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    require(args.operation.is_absolute(), 'Require an absolute operation directory')
    print(json.dumps(observe(json.loads(args.recipe.read_text()), args.operation)))


if __name__ == '__main__':
    main()
