"""Validate worker artifact identity before modifying an image builder."""
import hashlib
import re
import copy


def worker_artifact(path, sha256, version):
    if path is None and sha256 is None and version is None:
        return {}
    if path is None or sha256 is None or version is None:
        raise ValueError('--worker, --worker-sha256 and --worker-version must be supplied together')
    if not re.fullmatch(r'[a-fA-F0-9]{64}', sha256) or not re.fullmatch(r'v\d+\.\d+\.\d+\+k0s\.[a-zA-Z0-9.+-]+', version):
        raise ValueError('invalid worker hash or version')
    if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != sha256.lower():
        raise ValueError('worker artifact checksum mismatch')
    return {'workerSHA256': sha256.lower(), 'workerVersion': version}


def same_recipe(record, tunnel_sha256, worker):
    return record.get('tunnelSHA256') == tunnel_sha256 and all(record.get(k) == v for k, v in worker.items())


def supersede_cache_revision(record, expected, recipe, year, service_sha, worker, state, requested_at):
    """Replace an explicit pending cache intent, never prepared image evidence."""
    from images.windows.cache import validate_recipe
    previous = record.get('requestedCacheRecipe', {})
    if (state != 'stopped' or not record.get('revisionOf') or not record.get('revisionRequestedAt')
            or any(record.get(key) for key in ['preparedAt', 'generalizeRequestedAt', 'imageID'])
            or not re.fullmatch('[a-f0-9]{64}', expected)
            or previous.get('sha256') != expected
            or record.get('requestedSHA256') != service_sha
            or record.get('requestedWorker', {}) != worker):
        raise RuntimeError('Cache supersession requires a stopped unprepared revision and its exact previous intent')
    validate_recipe(previous, year)
    validate_recipe(recipe, year)
    if recipe['sha256'] == expected or recipe['runtime'] != previous['runtime']:
        raise RuntimeError('Cache supersession must change references while preserving the runtime')
    result = copy.deepcopy(record)
    result.setdefault('supersededCacheRevisions', []).append({
        'requestedCacheRecipe': copy.deepcopy(previous),
        'revisionRequestedAt': record['revisionRequestedAt'], 'supersededAt': requested_at})
    result['requestedCacheRecipe'] = copy.deepcopy(recipe)
    result['revisionRequestedAt'] = requested_at
    return result


def gpu_evidence(evidence, year):
    """Require native post-reboot driver evidence before AWS GPU capture.

    This is preparation evidence, never workload or license qualification.
    """
    if not isinstance(evidence, dict):
        raise ValueError('GPU verification evidence is missing')
    if (evidence.get('windowsBuild') != {'2022': 20348, '2025': 26100}[year]
            or evidence.get('verifiedAfterReboot') is not True
            or evidence.get('licenseScope') != 'aws'
            or not evidence.get('licenseSource')
            or not evidence.get('bootTime')
            or not re.fullmatch(r'[a-f0-9]{64}', str(evidence.get('packageSHA256', '')))
            or not evidence.get('driverVersion')):
        raise ValueError('GPU verification OS, reboot or recipe identity mismatch')
    gpus = evidence.get('gpus')
    if (not isinstance(gpus, list) or not gpus
            or evidence.get('gpuCount') != len(gpus)
            or any(not isinstance(gpu, dict)
                   or str(gpu.get('driverModel', '')).strip() != 'WDDM'
                   or str(gpu.get('driverVersion', '')).strip() != evidence['driverVersion']
                   for gpu in gpus)):
        raise ValueError('GPU verification requires the pinned driver in WDDM mode')
    return evidence


def gpu_base_identity(builder, base, tunnel_sha256, worker=None):
    """A GPU layer preserves the qualified CPU base's worker and tunnel."""
    if (not base.get('imageID') or builder.get('baseImageID') != base['imageID']
            or not base.get('tunnelSHA256')
            or tunnel_sha256 != base['tunnelSHA256'] or worker):
        raise ValueError('GPU preparation must retain the recorded CPU base image tunnel and worker; revise the CPU base first')
