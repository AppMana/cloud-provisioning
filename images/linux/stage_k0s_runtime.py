#!/usr/bin/env python3
"""Stage the linux runtime embedded in a verified k0s executable."""
import functools
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from runtime_payload import PROFILES, main, stage_runtime

COMPONENTS = PROFILES['linux'][0]
stage = functools.partial(stage_runtime, machine_os='linux')

if __name__ == '__main__':
    main('linux')
