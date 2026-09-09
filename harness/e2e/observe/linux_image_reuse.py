"""Validate native evidence that a fresh CAPI worker reused a baked k0s binary."""
import argparse
import datetime
import json
import math
import pathlib
import re


def evaluate(candidate, binding, observation):
    expected = candidate['preparation']
    launch = datetime.datetime.fromisoformat(binding['launchTime'])
    if launch.tzinfo is None:
        raise ValueError('launch timestamp requires a timezone')
    mtime = observation.get('mtime')
    # Parse the server separately: a containerd 2 client alone does not prove
    # that the worker is running a containerd 2 server.
    versions = dict(re.findall(r'(?m)^(Client|Server):\s*\n\s*Version:\s*(\S+)', observation.get('containerd', '')))
    nics = observation.get('physicalNICs', [])
    checks = dict(
        candidateImageMatches=binding.get('imageID') == candidate['candidateAMIID'],
        nodeReady=binding.get('nodeReady') is True,
        oneENI=type(binding.get('eniCount')) is int and binding['eniCount'] == 1,
        originalReceiptUnchanged=observation.get('receipt') == expected,
        binaryHashMatches=bool(re.fullmatch(r'[0-9a-f]{64}', expected['sha256'])) and observation.get('sha256') == expected['sha256'],
        versionMatches=observation.get('version') == expected['version'],
        executableMtimePredatesLaunch=type(mtime) in (int, float) and math.isfinite(mtime) and 0 <= mtime < launch.timestamp(),
        oneGuestPhysicalNIC=isinstance(nics, list) and len(nics) == 1 and isinstance(nics[0], str) and bool(nics[0]),
        containerdServer2=bool(re.fullmatch(r'v?2\.[0-9]+\.[0-9]+(?:[-+].*)?', versions.get('Server', ''))),
    )
    return dict(passed=all(checks.values()), checks=checks, containerdVersions=versions,
                binding=binding, scope='Baked executable reuse; not runtime/workload cache or lifecycle qualification')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate-report', type=pathlib.Path, required=True)
    parser.add_argument('--observation', type=pathlib.Path, required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    native = json.loads(args.observation.read_text())
    result = evaluate(json.loads(args.candidate_report.read_text()), native['binding'], native['observation'])
    with args.output.open('x') as stream:
        json.dump(result, stream, indent=2)
        stream.write('\n')
    if not result['passed']:
        raise SystemExit('Native executable reuse checks failed')


if __name__ == '__main__':
    main()
