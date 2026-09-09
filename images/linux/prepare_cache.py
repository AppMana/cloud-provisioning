#!/usr/bin/env python3
"""Build a fresh k0s/containerd 2 cache on an unjoined Linux amd64 VM.

The operation directory and cache must be new. Interrupted builds retain their
original logs and partial store for inspection; this command never replays them.
"""
import argparse
import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import subprocess
import time

DIGEST = re.compile(r'sha256:[a-f0-9]{64}')
REFERENCE = re.compile(r'[A-Za-z0-9][A-Za-z0-9._:/@+-]*')
WORKER = pathlib.Path('/usr/local/bin/k0s')
BIN = pathlib.Path('/var/lib/k0s/bin')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def inputs(recipe, directory):
    require(type(recipe.get('schemaVersion')) is int and recipe.get('schemaVersion') == 1 and recipe.get('machineOS') == 'linux'
            and recipe.get('architecture') == 'amd64', 'Require a Linux amd64 schema 1 recipe')
    require(re.fullmatch(r'[a-f0-9]{64}', recipe.get('workerSHA256', '')), 'Require worker SHA256')
    require(isinstance(recipe.get('images'), list) and recipe['images'], 'Require images')
    expected = {}
    for item in recipe['images']:
        require(isinstance(item, dict), 'Require image objects')
        digest = item.get('digest', '')
        require(isinstance(digest, str) and DIGEST.fullmatch(digest), 'Require image digest')
        aliases = item.get('aliases', [])
        require(isinstance(aliases, list) and aliases, 'Require explicit image aliases')
        for alias in aliases:
            require(isinstance(alias, str) and REFERENCE.fullmatch(alias)
                    and '://' not in alias and alias not in expected, 'Invalid or duplicate image alias')
            require('@' not in alias or alias.rsplit('@', 1)[1] == digest, 'Alias digest mismatch')
            expected[alias] = digest
        require(('archive' in item) != ('pullReference' in item), 'Choose archive or pinned pull')
        source = item.get('pullReference', item.get('reference'))
        require(source in aliases, 'Source must be declared in aliases')
        if 'pullReference' in item:
            require(source.endswith('@'+digest), 'Pull reference must be digest pinned')
        else:
            name = item['archive']
            require(isinstance(name, str) and pathlib.Path(name).name == name
                    and name not in ('', '.', '..'), 'Archive must be a staged basename')
            archive = directory/name
            require(not archive.is_symlink() and archive.is_file(), 'Require regular archive')
            require(sha256(archive) == item.get('archiveSHA256'), 'Archive hash mismatch')
    return expected


def runtime(recipe):
    require(os.geteuid() == 0, 'Require root on the builder VM')
    for service in ['k0sworker', 'k0scontroller', 'kubelet']:
        result = subprocess.check_output(['systemctl', 'show', service, '--property=LoadState', '--value'], text=True)
        require(result.strip() == 'not-found', 'Kubernetes service exists: '+service)
    for directory in [pathlib.Path('/etc/k0s'), pathlib.Path('/etc/kubernetes')]:
        require(not directory.exists() or (directory.is_dir() and not any(directory.iterdir())), 'Cluster configuration exists')
    require(subprocess.run(['pgrep', '-x', 'containerd'], capture_output=True).returncode == 1,
            'Require no running containerd')
    receipt = json.loads(pathlib.Path('/etc/cloud-provisioning-image/linux-runtime.json').read_text())
    require(not WORKER.is_symlink() and receipt['workerSHA256'] == recipe['workerSHA256'] == sha256(WORKER), 'Worker mismatch')
    require(WORKER.stat().st_mtime_ns == receipt['workerMtimeNs'], 'Worker timestamp changed')
    require({x['name'] for x in receipt['components']} == {'containerd', 'containerd-shim-runc-v2', 'runc'}, 'Unexpected runtime profile')
    for item in receipt['components']:
        path = BIN/item['name']
        require(not path.is_symlink() and sha256(path) == item['sha256'], 'Runtime bytes changed')
        require(path.stat().st_mtime_ns == receipt['workerMtimeNs']
                and stat.S_IMODE(path.stat().st_mode) == 0o750, 'Runtime metadata changed')


def build(recipe_path, operation, root, minimum_free_gib=8):
    recipe = json.loads(recipe_path.read_text())
    recipe_sha256 = sha256(recipe_path)
    expected = inputs(recipe, recipe_path.parent)
    runtime(recipe)
    for path in [operation, root]:
        require(path.is_absolute() and path == path.resolve(), 'Require absolute paths without symlinks')
        require(not path.exists(), 'Inspect the existing operation/cache; do not replay')
    require(operation not in root.parents and root not in operation.parents and operation != root,
            'Operation and cache must be separate directories')
    require(minimum_free_gib > 0 and shutil.disk_usage(root.parent).free >= minimum_free_gib * (1 << 30), 'Insufficient cache disk space')
    operation.mkdir(mode=0o700)
    def save(name, value):
        with (operation/name).open('x') as out:
            json.dump(value, out, indent=2)
            out.write('\n')
    save('intent.json', dict(recipe=recipe, recipeSHA256=recipe_sha256, cacheRoot=str(root)))
    root.mkdir(mode=0o700)
    state = pathlib.Path('/run')/('cldt-cache-build-'+str(os.getpid()))
    state.mkdir(mode=0o700)
    socket = str(state/'containerd.sock')
    config = operation/'containerd.toml'
    config.write_text('version = 3\ndisabled_plugins = ["io.containerd.grpc.v1.cri"]\n')
    ctr = [str(WORKER), 'ctr', '--address', socket, '--namespace', 'k8s.io']
    def call(args, label, timeout=60):
        with (operation/(label+'.stdout')).open('xb') as out, (operation/(label+'.stderr')).open('xb') as err:
            result = subprocess.run(ctr+args, stdout=out, stderr=err, timeout=timeout)
        save(label+'.exit.json', dict(exitCode=result.returncode))
        require(result.returncode == 0, 'Native command failed: '+label)
        return (operation/(label+'.stdout')).read_text()
    observation = dict(stops=[])
    for phase in ['before', 'after']:
        with (operation/(phase+'.log')).open('xb') as log:
            process = subprocess.Popen([str(BIN/'containerd'), '--root', str(root), '--state', str(state),
                '--address', socket, '--config', str(config)], stdout=log, stderr=subprocess.STDOUT,
                env=dict(os.environ, PATH=str(BIN)+':'+os.environ['PATH']))
            try:
                save(phase+'-process.json', dict(pid=process.pid))
                for _ in range(60):
                    require(process.poll() is None, 'Original runtime exited during startup')
                    if subprocess.run(ctr+['version'], capture_output=True, timeout=5).returncode == 0:
                        break
                    time.sleep(.5)
                else:
                    raise RuntimeError('Original runtime did not become ready')
                if phase == 'before':
                    require(not call(['images', 'list', '--quiet'], 'initial').strip(), 'Fresh cache contains images')
                    for index, item in enumerate(recipe['images']):
                        label = 'image-'+str(index)
                        source = item.get('pullReference', item.get('reference'))
                        if 'archive' in item:
                            archive = recipe_path.parent/item['archive']
                            require(sha256(archive) == item['archiveSHA256'], 'Archive changed before import')
                            call(['images', 'import', '--platform', 'linux/amd64', str(archive)], label+'-import', 600)
                        else:
                            call(['images', 'pull', '--platform', 'linux/amd64', source], label+'-pull', 600)
                        inventory = {row.split()[0]: row.split()[2] for row in call(['images', 'list'], label+'-inventory').splitlines()[1:] if len(row.split()) >= 3}
                        require(inventory.get(source) == item['digest'], 'Pulled/imported source digest differs')
                        for number, alias in enumerate(item['aliases']):
                            if alias in inventory:
                                require(inventory[alias] == item['digest'], 'Refuse alias retargeting')
                            else:
                                call(['images', 'tag', source, alias], label+'-alias-'+str(number))
                        save(label+'-complete.json', dict(source=source, digest=item['digest']))
                observation[phase] = call(['images', 'check'], phase+'-check')
                observation['ready'+phase.title()] = call(['images', 'check', '--quiet'], phase+'-ready')
                rows = {r.split()[0]: r.split()[2] for r in observation[phase].splitlines()[1:] if len(r.split()) >= 3}
                require(rows == expected, 'Unexpected or missing image digest')
                require(set(observation['ready'+phase.title()].splitlines()) == set(expected), 'Cache not complete and unpacked')
            finally:
                if process.poll() is None:
                    process.terminate()
                code = process.wait(timeout=30)
                stopped = dict(exitCode=code, originalProcessExited=True)
                save(phase+'-stopped.json', stopped)
                observation['stops'].append(stopped)
                require(code == 0, 'Original runtime did not stop cleanly')
    shutil.rmtree(state)
    observation['temporaryStateRemoved'] = True
    save('observation.json', observation)
    require(sha256(recipe_path) == recipe_sha256, 'Recipe changed during the original build')
    result = dict(recipeSHA256=recipe_sha256, cacheRoot=str(root), references=len(expected),
                  originalRuntimeProcessesExited=True, readyBeforeAndAfterRestart=True,
                  qualificationEligible=False, scope='Builder cache; independent verification and fresh-clone validation required')
    save('result.json', result)
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--recipe', type=pathlib.Path, required=True)
    p.add_argument('--operation', type=pathlib.Path, required=True)
    p.add_argument('--cache-root', type=pathlib.Path, default=pathlib.Path('/var/lib/k0s/containerd'))
    p.add_argument('--minimum-free-gib', type=int, default=8)
    a = p.parse_args()
    os.umask(0o077)
    print(json.dumps(build(a.recipe.resolve(), a.operation, a.cache_root, a.minimum_free_gib)))


if __name__ == '__main__':
    main()
