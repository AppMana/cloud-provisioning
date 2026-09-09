"""Evaluate Windows build-time cache readiness, separately from clone qualification."""
import argparse
import json
import pathlib
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))
from images.windows.cache import evaluate


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['snapshot', 'runtime', 'output']:
        parser.add_argument('--' + name, required=True, type=pathlib.Path)
    parser.add_argument('--image', required=True, action='append')
    parser.add_argument('--windows-version', required=True, choices=['2022', '2025'])
    args = parser.parse_args()
    result = evaluate(json.loads(args.snapshot.read_text(encoding='utf-8-sig')),
                      json.loads(args.runtime.read_text()), args.image, args.windows_version)
    args.output.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
    if not result['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
