"""Package the chart using the workflow's actual version arguments; never push."""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ".github/workflows/publish-chart.yml"


class ChartPublication(unittest.TestCase):
    def test_chart_packages_with_release_version(self):
        revision = os.environ.get("WORKFLOW_TEST_REVISION")
        source = subprocess.check_output(
            ["git", "show", f"{revision}:{WORKFLOW}"], cwd=ROOT, text=True
        ) if revision else (ROOT / WORKFLOW).read_text()
        commands = "\n".join(line for line in source.splitlines() if re.match(
            r"          (base_version=|test -n |helm package )", line
        ))
        self.assertIn("helm package", commands)
        expected_version = yaml.safe_load((ROOT / "charts/cloud-provisioning/Chart.yaml").read_text())["version"]
        with tempfile.TemporaryDirectory() as directory:
            script = textwrap.dedent(commands).replace("dist/chart", directory)
            subprocess.run(["bash", "-euo", "pipefail", "-c", script], cwd=ROOT, check=True)
            packages = list(Path(directory).glob("*.tgz"))
            self.assertEqual(len(packages), 1)
            chart = subprocess.check_output(
                ["helm", "show", "chart", str(packages[0])], text=True
            )
            version = re.search(r"^version: (.+)$", chart, re.M)[1]
            self.assertEqual(version, expected_version)
            self.assertRegex(chart, rf'(?m)^appVersion: "?{re.escape(expected_version)}"?$')


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


class AWSCredentials(unittest.TestCase):
    """The controller's AWS calls use the SDK's standard chain, fed by a
    Secret the operator names, either as environment keys or as files."""

    def render(self, *values):
        args = ["helm", "template", "credential-test", "charts/cloud-provisioning"]
        for value in values:
            args += ["--set-string", value]
        return subprocess.run(args, cwd=ROOT, capture_output=True, text=True)

    def test_environment_keys(self):
        result = self.render("aws.credentials.secretName=keys")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("envFrom:", result.stdout)
        self.assertIn('name: "keys"', result.stdout)
        self.assertNotIn("AWS_CONFIG_FILE", result.stdout)

    def test_files_mount_the_directory_for_the_sdk(self):
        result = self.render("aws.credentials.secretName=profiles", "aws.credentials.mount=files")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fsGroup: 65532", result.stdout)
        self.assertIn('secretName: "profiles"', result.stdout)
        self.assertIn("mountPath: /etc/cloud-provisioning/aws", result.stdout)
        self.assertIn("value: /etc/cloud-provisioning/aws/config", result.stdout)
        self.assertIn("value: /etc/cloud-provisioning/aws/credentials", result.stdout)
        self.assertNotIn("subPath:", result.stdout)
        self.assertNotIn("envFrom:", result.stdout)

    def test_rejects_an_unknown_mount(self):
        result = self.render("aws.credentials.secretName=profiles", "aws.credentials.mount=volume")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("aws.credentials.mount", result.stderr)


class ReceiptTLS(unittest.TestCase):
    """Receipt serving gets its certificate from cert-manager unless the
    operator names a Secret; the CA agents trust outlives serving renewals."""
    POLICY = json.dumps({"nativeCIDRs": ["192.0.2.0/24"], "ports": [51821], "journal": {
        "roundLifetime": "5m", "maxReceiptAge": "1m", "drainInterval": "1m", "receipts": {
            "bindAddress": ":9444", "certFile": "/etc/cloud-provisioning/receipts/tls.crt",
            "keyFile": "/etc/cloud-provisioning/receipts/tls.key", "imageIDs": {"amd64": "sha256:" + "a" * 64}}}})

    def render(self, *args):
        return subprocess.run(["helm", "template", "receipt-test", "charts/cloud-provisioning",
                               "--namespace", "appmana",
                               "--set-json", "tunnel.receivePreparation=" + self.POLICY, *args],
                              cwd=ROOT, capture_output=True, text=True)

    def test_cert_manager_issues_and_renews_the_serving_certificate(self):
        result = self.render("--api-versions", "cert-manager.io/v1")
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        certificates = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Certificate"}
        serving = certificates["cloud-provisioning-endpoint-controller-receipts-tls"]
        self.assertEqual(serving["spec"]["dnsNames"], ["cloud-provisioning-endpoint-controller-receipts.appmana.svc"])
        self.assertEqual(serving["spec"]["issuerRef"]["name"], "cloud-provisioning-endpoint-controller-receipts-ca")
        self.assertTrue(certificates["cloud-provisioning-endpoint-controller-receipts-ca"]["spec"]["isCA"])
        self.assertIn('\\"trustSecret\\":\\"cloud-provisioning-endpoint-controller-receipts-tls\\"', result.stdout)

    def test_an_operator_secret_is_used_as_given(self):
        result = self.render("--set", "tunnel.receiptTLSSecret=operator-tls")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("kind: Certificate", result.stdout)
        self.assertIn('\\"trustSecret\\":\\"operator-tls\\"', result.stdout)

    def test_without_either_rendering_fails(self):
        result = self.render()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cert-manager", result.stderr)


if __name__ == "__main__":
    unittest.main()
