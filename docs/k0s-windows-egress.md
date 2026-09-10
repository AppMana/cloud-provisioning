# k0s API egress with Windows workers

Mixed Windows/Linux k0s sites must validate API-server access to admission
webhooks separately from node readiness and Windows pod traffic. The tested
k0s 1.36.2 site uses Konnectivity for cluster egress. Its servers select agents
using `destHost,defaultRoute,default`; the native agents advertise their node IP.
A Service address does not match those node identities. Without a default-route
advertiser, the fallback can select a Windows agent that cannot reach the Service.

Kubernetes documents that [Windows hosts cannot access Service IPs](https://kubernetes.io/docs/tasks/debug/debug-cluster/windows/#network-troubleshooting).
The [Konnectivity strategies](https://pkg.go.dev/sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies)
distinguish destination-host matching, explicit default-route agents and random
healthy backends. Native Windows 2022/2025 host probes and agent logs recorded
webhook timeouts; Linux controller hosts reached the same webhook.

## Linux default-route addon

The harness can derive an additional Linux-only DaemonSet from the live distro
agent. It preserves the native mixed-OS agents and their node-IP identities,
which remain necessary for Windows kubelet access. The addon inherits the
native image, environment, service account and projected credential mounts.
It uses a distinct agent ID and health/admin ports 18095/18096, restricts placement
to explicit Linux hostnames, and permits one unavailable agent with no surge
pod during rolling updates. There must be at least two distinct selected hosts.

For a fresh single-NIC VM site, add `--k0s-linux-egress` to `go run ./cmd/lab`
with `--distro k0s --rig vm`. The flag is off by default and rejected for site
reuse or other distributions/rigs. It installs agents on the site controllers
after the bundled network is Ready and before CAPI installation. The installer
checks Linux placement and free ports, saves the native and generated manifests
in the work directory, and waits for each agent to connect to every controller.
The existing-site native validation below does not establish a fresh bringup
with this flag; that remains a separate matrix gate.

For an existing test site, first verify each selected Linux host can reach the
required webhook Services and endpoints. Confirm localhost 7132 is its native
k0s Konnectivity load balancer, ports 18095/18096 are free, and **every server**
uses `destHost,defaultRoute,default`. Render without mutating the cluster:

```sh
cd harness/e2e
kubectl --context TEST_CONTEXT -n kube-system get daemonset konnectivity-agent \
  -o json > native-agent.json
go run ./cmd/k0segress --native-daemonset native-agent.json \
  --nodes cp2,cp3 --output linux-egress.json
kubectl --context TEST_CONTEXT apply --dry-run=server -f linux-egress.json
kubectl --context TEST_CONTEXT apply -f linux-egress.json
kubectl --context TEST_CONTEXT -n kube-system rollout status \
  daemonset/cldt-konnectivity-default-route-linux --timeout=120s
```

Use a new output filename for every render; existing files are never overwritten.
The renderer rejects unsupported native argument/layout changes instead of
assuming another distribution version behaves the same way. The implementation
is in [egress.go](../harness/e2e/cluster/k0s/egress.go); its fixture is the observed
k0s 1.36.2 DaemonSet with runtime metadata removed.

## Validation and failure boundaries

Readiness alone is insufficient. Check host port 18095 metrics for
`konnectivity_network_proxy_agent_open_server_connections` equal to the controller
count. Exercise server dry-run CAPI Machine updates through each API server,
native exec into both Windows versions, and actual provisioning/removal.
Observe webhook sockets or dial logs to establish which agent carries traffic.
Preserve failures and the original pre-change baseline.

[Native two-agent evidence](validation/windows-konnectivity-ha-results.json)
contains 240 passing admission probes and 12 Windows exec checks across graceful
withdrawal of either temporary agent. Migration to the generated DaemonSet added
120 passing admission probes and six exec checks. Both final agents connected
to all three servers; all 13 Nodes were Ready. The temporary Deployments were
removed. These are agent withdrawal tests, not VM power-loss or partition tests.
An [identity-checked SIGKILL test](validation/windows-konnectivity-abrupt-results.json)
added 120 passing admission probes and six Windows exec checks. The killed
container exited 137 and restarted automatically in the same Pod, reconnecting
to all three servers. This establishes one process-crash recovery observation;
VM power loss, network partition and fresh-site gates remain open. A subsequent
[Linux CAPI worker cycle](validation/linux-capi-egress-results.json) completed
provisioning, 102 network checks and removal with the addon active, preserving
all 13 original Node identities. Its retained boot and image-staging failures
also produced new bootstrap-expiry and adoption gates. This single cycle does
not establish the full Windows lifecycle or endpoint-failure matrix.

A [later reconnection check](validation/windows-konnectivity-reconnection-results.json)
found both Windows test workers unreachable through one API server while the
other two could execute commands. The Windows 2022 agent had two server
connections; recreating each affected native agent restored three connections
and all six exec paths without changing Node or workload identities. The cause
of the missing connection remains unresolved. Check every API server before
network qualification. The post-recovery matrix retained two Windows UDP
timeouts at 1,800-byte payloads, so exec recovery does not qualify UDP reliability.

The [initial canary](validation/windows-konnectivity-default-route-results.json)
established actual Linux-owned webhook sockets. Its immediate ten-probe baseline
also passed, so neither observation quantifies a failure-rate improvement.
A log query against the older Windows 2025 node failed with a kubelet dial timeout;
replacement-node exec success does not establish all-node health.

When all default-route agents disappear, the server's `default` fallback can
again select an unsuitable Windows agent. To remove the addon:

```sh
kubectl --context TEST_CONTEXT -n kube-system delete \
  daemonset cldt-konnectivity-default-route-linux
```

The addon does not resolve Windows VFP fragmentation or intermittent GPU process
startup. These observations do not qualify Windows support for release.
