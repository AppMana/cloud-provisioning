#!/usr/bin/env python3
"""Render an IAM JSON template using explicitly supplied, non-secret values."""
import argparse
import json
import pathlib
import re

p = argparse.ArgumentParser()
p.add_argument('template', type=pathlib.Path)
p.add_argument('values', type=pathlib.Path, help='JSON object containing template variables')
a = p.parse_args()
values = json.loads(a.values.read_text())
def expand(match):
    key = match.group(1)
    value = values.get(key)
    if not isinstance(value, str) or not value or any(c in value for c in '*?${}'):
        p.error('missing or unsafe value for ' + key)
    # JSON-escape the value without introducing another pair of quotes.
    return json.dumps(value)[1:-1]
raw = re.sub(r'\$\{([A-Z_]+)\}', expand, a.template.read_text())
print(json.dumps(json.loads(raw), indent=2))
