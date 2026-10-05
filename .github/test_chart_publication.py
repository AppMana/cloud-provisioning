"""Package the chart using the workflow's actual version arguments; never push."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ".github/workflows/publish-chart.yml"


class ChartPublication(unittest.TestCase):
    def test_different_sources_have_different_chart_versions(self):
        revision = os.environ.get("WORKFLOW_TEST_REVISION")
        source = subprocess.check_output(
            ["git", "show", f"{revision}:{WORKFLOW}"], cwd=ROOT, text=True
        ) if revision else (ROOT / WORKFLOW).read_text()
        commands = "\n".join(line for line in source.splitlines() if re.match(
            r"          (base_version=|test -n |chart_version=|helm package )", line
        ))
        self.assertIn("helm package", commands)
        versions = []
        for commit in ("a" * 40, "b" * 40):
            with tempfile.TemporaryDirectory() as directory:
                script = textwrap.dedent(commands).replace(
                    "${{ github.event.workflow_run.head_sha }}", commit
                ).replace("/tmp/chart", directory)
                subprocess.run(["bash", "-euo", "pipefail", "-c", script],
                               cwd=ROOT, env={**os.environ, "sha": commit[:7]}, check=True)
                packages = list(Path(directory).glob("*.tgz"))
                self.assertEqual(len(packages), 1)
                chart = subprocess.check_output(
                    ["helm", "show", "chart", str(packages[0])], text=True
                )
                version = re.search(r"^version: (.+)$", chart, re.M)[1]
                self.assertIn(commit, version)
                self.assertRegex(chart, rf'(?m)^appVersion: "?{commit[:7]}"?$')
                versions.append(version)
        self.assertNotEqual(*versions)


if __name__ == "__main__":
    unittest.main()
