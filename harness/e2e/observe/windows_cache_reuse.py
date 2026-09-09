"""Qualify declared Windows cache reuse on a fresh, identity-bound worker.

Kubelet cache-hit events do not prove zero registry access. Runtime-only images
(such as pause) are checked separately from declared Pod container references.
"""
import pathlib
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))
from images.windows.cache import validate_recipe


def evaluate(node, pods, events, native, recipe, node_uid):
    year = {20348: '2022', 26100: '2025'}[recipe['windowsBuild']]
    validate_recipe(recipe, year)
    targets = {ref: ref.split('@')[1] for ref in recipe['images']}
    targets.update({a['reference']: a['image'].split('@')[1] for a in recipe.get('aliases', [])})
    components = {c['name']: c['sha256'] for c in recipe['runtime']['components']}
    metadata = node.get('metadata', {})
    checks = {
        'sameWindowsNode': bool(node_uid) and metadata.get('uid') == node_uid and
            metadata.get('labels', {}).get('kubernetes.io/os') == 'windows' and not metadata.get('deletionTimestamp'),
        'nativeIdentity': native.get('build') == recipe['windowsBuild'] and native.get('nics') == 1 and
            native.get('workerSHA256') == recipe['runtime']['workerSHA256'] and
            native.get('runtimeSHA256') == components['containerd.exe'],
        'declaredReferencesReady': set(targets) <= set(native.get('registeredImages', [])) and
            set(targets) <= set(native.get('readyImages', [])),
        'declaredReferenceTargets': all(native.get('imageTargets', {}).get(ref) == target for ref, target in targets.items()),
    }
    records = []
    identities = set()
    valid_pods = bool(pods.get('items'))
    for pod in pods.get('items', []):
        meta = pod['metadata']; ann = meta.get('annotations', {})
        ids = {meta.get('uid')}
        valid_pods &= bool(meta.get('uid')) and pod.get('spec', {}).get('nodeName') == metadata.get('name')
        if ann.get('kubernetes.io/config.mirror'):
            mirror = ann['kubernetes.io/config.mirror']
            valid_mirror = mirror == ann.get('kubernetes.io/config.hash') and any(
                owner.get('kind') == 'Node' and owner.get('uid') == node_uid
                for owner in meta.get('ownerReferences', []))
            valid_pods &= valid_mirror
            if valid_mirror:
                ids.add(mirror)
        keys = {(meta.get('namespace'), meta.get('name'), uid) for uid in ids}
        valid_pods &= not bool(identities & keys)
        identities.update(keys)
        pod_events = [e for e in events.get('items', []) if event_identity(e) in keys]
        for kind, status_kind in [('initContainers', 'initContainerStatuses'), ('containers', 'containerStatuses')]:
            statuses = {s['name']: s for s in pod.get('status', {}).get(status_kind, [])}
            for container in pod.get('spec', {}).get(kind, []):
                ref = container['image']; name = container['name']; target = targets.get(ref)
                field = 'spec.' + kind + '{' + name + '}'
                matching = [e for e in pod_events if e.get('involvedObject', {}).get('fieldPath') == field]
                hit = any(e.get('reason') == 'Pulled' and
                          ('Container image "' + ref + '" already present on machine') in e.get('message', '')
                          for e in matching)
                image_id = statuses.get(name, {}).get('imageID', '')
                records.append({'pod': meta['name'], 'podUID': meta['uid'], 'container': name,
                    'kind': kind, 'image': ref, 'declared': target is not None,
                    'ifNotPresent': container.get('imagePullPolicy') == 'IfNotPresent',
                    'resolvedTargetMatches': bool(target) and image_id.endswith('@' + target),
                    'cacheHitEvent': hit, 'pullingEvent': any(e.get('reason') == 'Pulling' for e in matching)})
    checks['podIdentityBindings'] = bool(valid_pods)
    for field in ['declared', 'ifNotPresent', 'resolvedTargetMatches', 'cacheHitEvent']:
        checks[field] = bool(records) and all(r[field] for r in records)
    checks['noObservedPodImagePulls'] = bool(records) and not any(
        e.get('reason') == 'Pulling' and event_identity(e) in identities for e in events.get('items', []))
    extra_targets = sorted(set(native.get('imageTargets', {}).values()) - set(targets.values()))
    reused = all(checks.values())
    return {'declaredCacheReused': reused, 'completeStartupCache': reused and not extra_targets,
            'checks': checks, 'containers': records, 'undeclaredRuntimeImageTargets': extra_targets,
            'additionalReferences': sorted(set(native.get('registeredImages', [])) - set(targets)),
            'scope': 'Observed Pod image cache reuse and native runtime inventory; not proof of zero registry access or startup performance'}


def event_identity(event):
    obj = event.get('involvedObject', {})
    return obj.get('namespace'), obj.get('name'), obj.get('uid')
