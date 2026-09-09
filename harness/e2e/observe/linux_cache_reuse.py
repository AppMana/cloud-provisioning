"""Validate fresh-worker cache reuse from native content and snapshot evidence."""
import datetime

from linux_cache import expected_images, inventory


def timestamp(value):
    parsed = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('Require timezone-qualified native timestamps')
    return parsed.timestamp()


def evaluate(candidate_image, recipe, binding, contracts, observation):
    expected = expected_images(recipe)
    targets = set(expected.values())
    launch = timestamp(binding['launchTime'])
    required_blobs = set()
    for digest, contract in contracts.items():
        blobs = contract['requiredBlobs']
        if digest not in blobs or len(blobs) != len(set(blobs)):
            raise ValueError('Invalid descriptor closure')
        required_blobs.update(blobs)
    rows = inventory(observation['imagesCheck'])
    blobs = observation['blobs']
    snapshots = observation['snapshots']
    capture = observation['captureReceipt']
    runtime_paths = {'/usr/local/bin/k0s', '/var/lib/k0s/bin/containerd',
                     '/var/lib/k0s/bin/containerd-shim-runc-v2', '/var/lib/k0s/bin/runc'}

    def predates(value):
        return type(value) is int and 0 < value < launch * 1_000_000_000

    checks = dict(
        candidateMatches=binding['imageID'] == candidate_image,
        descriptorTargetsMatch=set(contracts) == targets,
        cacheReceiptMatches=observation['images'] == expected,
        builderGatePassed=capture['validation']['passed'] is True,
        requiredReferencesReady=set(expected) <= set(observation['ready'].splitlines()),
        contentCompleteAndUnpacked=all(
            rows.get(name, {}).get('digest') == digest and
            rows[name]['complete'] and rows[name]['unpacked'] for name, digest in expected.items()),
        allRequiredBlobsObserved=bool(required_blobs) and set(blobs) == required_blobs,
        contentPredatesLaunch=bool(blobs) and all(
            predates(blob['ctimeNs']) and predates(blob['mtimeNs']) for blob in blobs.values()),
        snapshotTargetsMatch=set(snapshots) == targets,
        committedSnapshotsReused=all(
            digest in snapshots and snapshots[digest]['chainID'] == contract['chainID'] and
            snapshots[digest]['info']['Name'] == contract['chainID'] and
            snapshots[digest]['info']['Kind'] == 'Committed' and
            0 < timestamp(snapshots[digest]['info']['Created']) < launch and
            0 < timestamp(snapshots[digest]['info']['Updated']) < launch
            for digest, contract in contracts.items()),
        runtimeFileIdentitiesUnchanged=(set(observation['fileIdentities']) == runtime_paths and
            observation['fileIdentities'] == capture['fileIdentities']),
    )
    return dict(passed=all(checks.values()), checks=checks, references=len(expected),
                blobs=len(blobs), snapshotChains=len(snapshots), qualificationEligible=False,
                scope='Fresh-worker cache reuse before harness imports; not zero registry access, startup savings or lifecycle qualification')
