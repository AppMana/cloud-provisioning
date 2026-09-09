"""Observe an existing ordinary-pod TCP, Service, DNS and UDP matrix.

Requires working Kubernetes exec (including the distro's control-plane proxy).
Does not provision machines, install a CNI, or infer identity from pod names.
"""

import argparse
import datetime
import hashlib
import json
import pathlib
import subprocess
import urllib.parse

from udp import evaluate


CURL = {"linux": "curl", "windows": r"C:\curl\curl.exe"}
DEFAULT_UDP_SIZES = (64, 1280, 1340, 1400, 1800)


class Kubectl:
    def __init__(self, server, bastion, namespace):
        self.command = ["docker", "exec", "-i", bastion, "kubectl",
                        "--server=" + server, "-n", namespace]

    def get(self, kind, name):
        result = subprocess.run(self.command + ["get", kind, name, "-o", "json"],
                                capture_output=True, timeout=30, check=True)
        return json.loads(result.stdout)

    def curl(self, source, arguments, body=None, timeout=30):
        stdout, code, _ = self.curl_result(source, arguments, body, timeout)
        return stdout, code

    def curl_result(self, source, arguments, body=None, timeout=30):
        """Include executor diagnostics so failed observations remain explainable."""
        try:
            result = subprocess.run(self.command + ["exec", "-i", source["pod"],
                "-c", source["container"], "--", CURL[source["os"]],
                "--silent", "--show-error", "--fail", "--max-time", str(timeout),
                "--noproxy", "*"] + arguments, input=body,
                capture_output=True, timeout=timeout + 15)
            return result.stdout, result.returncode, result.stderr.decode("utf-8", errors="replace")
        except subprocess.TimeoutExpired:
            return b"", 124, "Kubernetes exec observer timed out"


def ready(obj):
    return (not obj["metadata"].get("deletionTimestamp") and
            any(c["type"] == "Ready" and c["status"] == "True"
                for c in obj.get("status", {}).get("conditions", [])))


def snapshot(kube, targets):
    """Bind the observations to explicit Node UIDs and live container identities."""
    if len(targets) < 2 or len({t[0] for t in targets}) != len(targets):
        raise ValueError("at least two distinct target pods are required")
    result = []
    for name, node_uid in targets:
        pod = kube.get("pod", name)
        node = kube.get("node", pod["spec"]["nodeName"])
        service = kube.get("service", name)
        if not node_uid or node["metadata"]["uid"] != node_uid:
            raise ValueError("target Node identity changed: " + name)
        if not ready(pod) or not ready(node):
            raise ValueError("target is not Ready: " + name)
        containers = pod["spec"]["containers"]
        contexts = [pod["spec"].get("securityContext", {})] + [
            c.get("securityContext", {}) for c in containers]
        if (pod["spec"].get("hostNetwork") or len(containers) != 1 or
                any(c.get("windowsOptions", {}).get("hostProcess") for c in contexts)):
            raise ValueError("target must be a single-container ordinary pod: " + name)
        os_name = node["metadata"]["labels"]["kubernetes.io/os"]
        if os_name not in CURL:
            raise ValueError("unsupported guest OS: " + os_name)
        container = pod["status"]["containerStatuses"][0]
        if not container.get("ready") or not container.get("containerID"):
            raise ValueError("target container is not ready: " + name)
        address = pod["status"].get("podIP")
        service_ip = service["spec"].get("clusterIP")
        if not address or not service_ip or service_ip == "None":
            raise ValueError("pod and Service addresses are required")
        result.append(dict(pod=name, podUID=pod["metadata"]["uid"],
            node=node["metadata"]["name"], nodeUID=node_uid, os=os_name,
            container=containers[0]["name"], containerID=container["containerID"],
            podIP=address, serviceUID=service["metadata"]["uid"], serviceIP=service_ip))
    if len({r["nodeUID"] for r in result}) != len(result):
        raise ValueError("matrix targets must occupy distinct nodes")
    return result


def host(address):
    return "[" + address + "]" if ":" in address else address


def run(kube, targets, namespace, tries=10, udp_sizes=DEFAULT_UDP_SIZES, udp_only=False):
    if not 1 <= tries <= 100:
        raise ValueError("UDP tries must be between 1 and 100")
    udp_sizes = tuple(udp_sizes)
    if (not udp_sizes or len(set(udp_sizes)) != len(udp_sizes) or
            any(type(size) is not int or not 1 <= size <= 2043 for size in udp_sizes)):
        raise ValueError("UDP sizes must be distinct integers from 1 to 2043 (agnhost buffer minus echo prefix)")
    before = snapshot(kube, targets)
    rows = []
    for source in before:
        for target in before:
            if source == target:
                continue
            identity = dict(source=source["pod"], target=target["pod"])
            for size in (() if udp_only else (1024, 16384, 1048576)):
                payload = b"x" * size
                body, code = kube.curl(source, ["--data-urlencode", "msg@-",
                    "http://" + host(target["podIP"]) + ":8080/echo"], payload)
                rows.append(dict(identity, kind="tcp", payloadBytes=size,
                    exitCode=code, receivedBytes=len(body),
                    expectedSHA256=hashlib.sha256(payload).hexdigest(),
                    receivedSHA256=hashlib.sha256(body).hexdigest(),
                    ok=code == 0 and body == payload))
            for kind, address in (() if udp_only else (("service", host(target["serviceIP"])),
                    ("dns", target["pod"] + "." + namespace + ".svc.cluster.local"))):
                body, code = kube.curl(source, ["http://" + address + ":8080/hostname"])
                rows.append(dict(identity, kind=kind, exitCode=code,
                    ok=code == 0 and body.strip() == target["pod"].encode()))
            for size in udp_sizes:
                payload = "x" * size
                query = urllib.parse.urlencode(dict(host=target["podIP"], port=8081,
                    protocol="udp", tries=tries, request="echo " + payload))
                started = datetime.datetime.now(datetime.timezone.utc).isoformat()
                body, code = kube.curl(source,
                    ["http://127.0.0.1:8080/dial?" + query], timeout=tries * 5 + 10)
                row = evaluate(body.decode("utf-8-sig", errors="replace"), payload, tries, code)
                rows.append(dict(row, **identity, kind="udp", payloadBytes=size,
                                 startedAt=started,
                                 finishedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                                 rawBody=body.decode("utf-8-sig", errors="replace")))
    try:
        stable = snapshot(kube, targets) == before
        identity_error = None
    except (ValueError, KeyError, subprocess.SubprocessError) as error:
        stable, identity_error = False, type(error).__name__
    return dict(observedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        scope=("Distinct-node ordinary-pod " + ("UDP sweep" if udp_only else "matrix") +
               " through Kubernetes exec; excludes policy and failover"),
        parameters=dict(udpTries=tries, udpPayloadBytes=list(udp_sizes), udpOnly=udp_only),
        targets=before, checks=rows, identitiesStable=stable, identityError=identity_error,
        ok=stable and bool(rows) and all(row["ok"] for row in rows))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-server", required=True)
    parser.add_argument("--bastion", required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--target", action="append", required=True, metavar="POD=NODE_UID")
    parser.add_argument("--tries", type=int, default=10)
    parser.add_argument("--udp-payload-bytes", action="append", type=int,
                        help="repeat for each UDP payload size; 1..2043, excluding the echo prefix")
    parser.add_argument("--udp-only", action="store_true", help="run only the directed UDP sweep")
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    if args.output.exists():
        parser.error("use a new evidence path")
    if any("=" not in target for target in args.target):
        parser.error("targets require POD=NODE_UID")
    targets = [tuple(target.split("=", 1)) for target in args.target]
    report = run(Kubectl(args.api_server, args.bastion, args.namespace),
                 targets, args.namespace, args.tries,
                 args.udp_payload_bytes if args.udp_payload_bytes is not None else DEFAULT_UDP_SIZES,
                 args.udp_only)
    with args.output.open("x") as output:
        json.dump(report, output, indent=2)
        output.write("\n")
    print(json.dumps(dict(ok=report["ok"], checks=len(report["checks"]),
                          passed=sum(c["ok"] for c in report["checks"]),
                          identitiesStable=report["identitiesStable"])))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
