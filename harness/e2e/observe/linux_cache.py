"""Validate native containerd cache evidence before and after a builder restart."""
import argparse
import json
import pathlib
import re

DIGEST = re.compile(r'sha256:[a-f0-9]{64}')
COUNTS = re.compile(r'\((\d+)/(\d+)\)')
REFERENCE = re.compile(r'[A-Za-z0-9][A-Za-z0-9._:/@+-]*')


def expected_images(recipe):
    if not isinstance(recipe, dict) or type(recipe.get('schemaVersion')) is not int or recipe.get('schemaVersion') != 1 or recipe.get('machineOS') != 'linux' or recipe.get('architecture') != 'amd64':
        raise ValueError('Require the tested Linux amd64 cache recipe profile')
    expected = {}
    images = recipe.get('images', [])
    if not isinstance(images, list) or not images:
        raise ValueError('Require a nonempty image recipe')
    for image in images:
        if not isinstance(image, dict):
            raise ValueError('Require image objects')
        digest = image.get('digest', '')
        if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
            raise ValueError('Require immutable image digests')
        aliases = image.get('aliases', [])
        if not isinstance(aliases, list) or not aliases:
            raise ValueError('Require explicit runtime references')
        for alias in aliases:
            if not isinstance(alias, str) or not REFERENCE.fullmatch(alias) or '://' in alias or alias in expected:
                raise ValueError('Invalid or duplicate image reference')
            if '@' in alias and alias.rsplit('@', 1)[1] != digest:
                raise ValueError('Digest reference disagrees with its target')
            expected[alias] = digest
    return expected


def inventory(text):
    rows = {}
    for line in text.splitlines():
        parts = line.split()
        if not parts or parts[0] == 'REF':
            continue
        if len(parts) < 6 or not DIGEST.fullmatch(parts[2]) or parts[-1] not in ['true', 'false']:
            raise ValueError('Malformed native images-check output')
        name = parts[0]
        if name in rows:
            raise ValueError('Duplicate native image observation')
        count = COUNTS.fullmatch(parts[4])
        complete = (parts[3] == 'complete' and count is not None
                    and int(count[1]) == int(count[2]) and int(count[2]) > 0)
        rows[name] = dict(digest=parts[2], complete=complete, unpacked=parts[-1] == 'true')
    return rows


def evaluate(recipe, observation):
    expected = expected_images(recipe)
    phases = {}
    for phase in ['before', 'after']:
        rows = inventory(observation[phase])
        quiet = observation['ready'+phase.title()].splitlines()
        phases[phase] = dict(
            inventoryMatches=set(rows) == set(expected),
            digestsMatch=all(rows.get(name, {}).get('digest') == digest for name, digest in expected.items()),
            contentComplete=all(rows.get(name, {}).get('complete') is True for name in expected),
            unpacked=all(rows.get(name, {}).get('unpacked') is True for name in expected),
            quietAgrees=len(quiet) == len(set(quiet)) and set(quiet) == set(expected),
        )
    stops = observation.get('stops', [])
    stopped = (len(stops) == 2 and all(type(stop.get('exitCode')) is int and stop['exitCode'] == 0
               and stop.get('originalProcessExited') is True for stop in stops))
    checks = dict(originalRuntimesStopped=stopped, temporaryStateRemoved=observation.get('temporaryStateRemoved') is True)
    passed = all(checks.values()) and all(all(phase.values()) for phase in phases.values())
    return dict(passed=passed, phases=phases, checks=checks, references=len(expected),
                qualificationEligible=False, scope='Builder cache only; capture and fresh-clone startup remain separate gates')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--recipe', type=pathlib.Path, required=True)
    parser.add_argument('--observation', type=pathlib.Path, required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    result = evaluate(json.loads(args.recipe.read_text()), json.loads(args.observation.read_text()))
    with args.output.open('x') as stream:
        json.dump(result, stream, indent=2)
        stream.write('\n')
    if not result['passed']:
        raise SystemExit('Original builder cache evidence did not pass')


if __name__ == '__main__': main()
