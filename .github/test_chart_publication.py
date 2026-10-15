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


class ImageReferences(unittest.TestCase):
    def render(self, values):
        args = ["helm", "template", "image-test", "charts/cloud-provisioning",
                "--set", "cluster-api-operator.enabled=false",
                "--show-only", "templates/deployment.yaml"]
        for key, value in values.items():
            args += ["--set-string", f"{key}={value}"]
        return subprocess.run(args, cwd=ROOT, text=True, capture_output=True)

    def test_digests_pin_controller_and_reconciled_dialers(self):
        values = {}
        expected = []
        for component, digit in [("image", "a"), ("dialerImage", "b")]:
            repository = f"registry.example:5443/release/{component.lower()}"
            digest = "sha256:" + digit * 64
            values.update({f"{component}.repository": repository,
                           f"{component}.digest": digest,
                           f"{component}.tag": "ignored-mutable-tag"})
            expected.append(f"{repository}@{digest}")
        result = self.render(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"image: {expected[0]}", result.stdout)
        self.assertIn(f'"--dialer-image={expected[1]}"', result.stdout)
        self.assertNotIn("ignored-mutable-tag", result.stdout)

    def test_invalid_digest_and_ambiguous_repository_fail_rendering(self):
        for component in ["image", "dialerImage"]:
            for digest in ["sha256:abc", "sha256:" + "A" * 64,
                           "sha512:" + "a" * 64, "sha256:" + "g" * 64]:
                with self.subTest(component=component, digest=digest):
                    result = self.render({f"{component}.digest": digest})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("image digest must be", result.stderr)
            for repository in ["registry.example/image:tag",
                               "registry.example/image@sha256:" + "a" * 64]:
                with self.subTest(component=component, repository=repository):
                    result = self.render({f"{component}.digest": "sha256:" + "a" * 64,
                                          f"{component}.repository": repository})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("repository must not include", result.stderr)

    def test_tag_only_values_keep_existing_behavior(self):
        result = self.render({"image.tag": "controller-tag", "dialerImage.tag": "dialer-tag"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("image: ghcr.io/appmana/cloud-provisioning-endpoint-controller:controller-tag", result.stdout)
        self.assertIn('"--dialer-image=ghcr.io/appmana/cloud-provisioning-dialer:dialer-tag"', result.stdout)

    def test_nonstring_digest_cannot_silently_fall_back_to_tag(self):
        for component in ["image", "dialerImage"]:
            for value in ["false", "0"]:
                with self.subTest(component=component, value=value):
                    result = subprocess.run([
                        "helm", "template", "image-test", "charts/cloud-provisioning",
                        "--set", "cluster-api-operator.enabled=false",
                        "--set", f"{component}.digest={value}"],
                        cwd=ROOT, text=True, capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("image digest must be a string", result.stderr)


class ObserverCredentialMount(unittest.TestCase):
    def render(self, *values):
        args = ["helm", "template", "credential-test", "charts/cloud-provisioning"]
        for value in values:
            args += ["--set-string", value]
        return subprocess.run(args, cwd=ROOT, capture_output=True, text=True)

    def test_rotating_secret_directory_and_nonroot_access(self):
        result = self.render("awsPodObserver.configMap=scope", "awsPodObserver.credentialsFileSecret=rotating")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fsGroup: 65532", result.stdout)
        self.assertIn("defaultMode: 0440", result.stdout)
        self.assertIn('secretName: "rotating"', result.stdout)
        self.assertIn("key: credentials.json", result.stdout)
        self.assertIn("mountPath: /etc/cloud-provisioning/aws-credentials", result.stdout)
        self.assertIn("--aws-pod-credentials-file=/etc/cloud-provisioning/aws-credentials/credentials.json", result.stdout)
        self.assertNotIn("subPath:", result.stdout)
        self.assertNotIn("envFrom:", result.stdout)

    def test_rejects_ambiguous_or_unused_credential_source(self):
        for values in [
            ["awsPodObserver.credentialsFileSecret=rotating"],
            ["awsPodObserver.configMap=scope", "awsPodObserver.credentialsFileSecret=rotating", "awsPodObserver.credentialsSecret=environment"],
        ]:
            with self.subTest(values=values):
                self.assertNotEqual(self.render(*values).returncode, 0)


if __name__ == "__main__":
    unittest.main()
