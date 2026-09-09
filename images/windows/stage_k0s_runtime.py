#!/usr/bin/env python3
"""Stage the windows runtime embedded in a verified k0s executable."""
import functools
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from runtime_payload import PROFILES, main, stage_runtime

COMPONENTS = PROFILES['windows'][0]
stage = functools.partial(stage_runtime, machine_os='windows')

if __name__ == '__main__':
    main('windows')
