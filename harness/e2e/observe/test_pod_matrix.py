import json
import unittest
import urllib.parse

from pod_matrix import run, snapshot


class Cluster:
    targets = [("linux", "node-linux"), ("win2022", "node-win2022"),
               ("win2025", "node-win2025")]

    def __init__(self, failure=None):
        self.failure = failure
        self.calls = 0

    def get(self, kind, name):
        metadata = dict(name=name, uid=kind + "-" + name)
        status = dict(conditions=[dict(type="Ready", status="True")])
        if kind == "node":
            metadata["labels"] = {"kubernetes.io/os": "windows" if name.startswith("win") else "linux"}
            return dict(metadata=metadata, status=status)
        index = [t[0] for t in self.targets].index(name)
        if kind == "service":
            return dict(metadata=metadata, spec=dict(clusterIP="10.96.0." + str(index + 1)))
        status.update(podIP="10.244.0." + str(index + 1), containerStatuses=[
            dict(name="serve", ready=True, containerID="container-" + name +
                 ("-replacement" if self.failure == "restart" and self.calls else ""))])
        return dict(metadata=metadata, status=status, spec=dict(nodeName=name,
            hostNetwork=self.failure == "hostNetwork",
            containers=[dict(name="serve", securityContext=dict(windowsOptions=dict(
                hostProcess=self.failure == "hostProcess")))]))

    def curl(self, source, arguments, body=None, timeout=30):
        self.calls += 1
        url = urllib.parse.urlparse(arguments[-1])
        if url.path == "/echo":
            return body, 28 if self.failure == "exit" else 0
        if url.path == "/hostname":
            address = url.hostname
            if address.startswith("10."):
                name = self.targets[int(address.rsplit(".", 1)[1]) - 1][0]
            else:
                name = address.split(".")[0]
            return name.encode(), 0
        query = urllib.parse.parse_qs(url.query)
        responses = [query["request"][0][5:]] * int(query["tries"][0])
        return json.dumps(dict(responses=None if self.failure == "null" else responses)).encode(), 0


class MatrixTest(unittest.TestCase):
    def test_boundary_sweep_sends_exact_request_sizes_and_attempts(self):
        class BoundaryCluster(Cluster):
            def curl(self, source, arguments, body=None, timeout=30):
                query = urllib.parse.parse_qs(urllib.parse.urlparse(arguments[-1]).query)
                self.requests.append((len(query['request'][0].encode()), int(query['tries'][0])))
                return super().curl(source, arguments, body, timeout)
        cluster = BoundaryCluster()
        cluster.requests = []
        report = run(cluster, cluster.targets[1:], 'test', tries=100,
                     udp_sizes=(1337, 1338), udp_only=True)
        self.assertTrue(report['ok'])
        self.assertEqual(cluster.requests, [(1342, 100), (1343, 100)] * 2)
        self.assertEqual(len(report['checks']), 4)
        self.assertTrue(all(c['kind'] == 'udp' and c['startedAt'] <= c['finishedAt']
                            for c in report['checks']))
        self.assertTrue(report['parameters']['udpOnly'])

    def test_invalid_sizes_fail_before_cluster_access(self):
        for sizes in ((), (0,), (2044,), (1337, 1337), (True,)):
            with self.subTest(sizes=sizes), self.assertRaises(ValueError):
                run(None, Cluster.targets, 'test', udp_sizes=sizes)

    def test_complete_three_node_matrix(self):
        cluster = Cluster()
        report = run(cluster, cluster.targets, "test")
        self.assertTrue(report["ok"])
        self.assertEqual(len(report["checks"]), 60)
        self.assertEqual(sum(c["kind"] == "tcp" for c in report["checks"]), 18)
        self.assertEqual(sum(c["kind"] == "udp" for c in report["checks"]), 30)
        self.assertEqual({t["os"] for t in report["targets"]}, {"windows", "linux"})

    def test_matching_body_cannot_hide_executor_failure(self):
        report = run(Cluster("exit"), Cluster.targets, "test")
        self.assertFalse(report["ok"])
        self.assertTrue(all(not c["ok"] for c in report["checks"] if c["kind"] == "tcp"))

    def test_null_udp_does_not_pass(self):
        report = run(Cluster("null"), Cluster.targets, "test")
        self.assertFalse(report["ok"])

    def test_container_replacement_invalidates_successful_traffic(self):
        report = run(Cluster("restart"), Cluster.targets, "test")
        self.assertTrue(all(c["ok"] for c in report["checks"]))
        self.assertFalse(report["identitiesStable"])
        self.assertFalse(report["ok"])

    def test_rejects_host_pods_and_wrong_node_identity(self):
        for mode in ("hostNetwork", "hostProcess"):
            with self.subTest(mode=mode), self.assertRaises(ValueError):
                snapshot(Cluster(mode), Cluster.targets)
        with self.assertRaises(ValueError):
            snapshot(Cluster(), [("linux", "stale"), ("win2022", "node-win2022")])


if __name__ == "__main__":
    unittest.main()
