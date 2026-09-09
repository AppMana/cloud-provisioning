# MicroK8s native fixture

`microk8s-1.34.9.json` projects observations from an Ubuntu 22.04 single-NIC
QEMU VM running snap revision 9063. The snap table and kubelet server line are
preserved verbatim; node information includes only software and architecture
fields. Calico images come from the bundled DaemonSet. Source hashes bind each
projection to the private raw observation. No join credentials are included.

`worker_test.go` uses the native observations for version, CRI socket and local
API assertions, with deliberately changed versions, missing files and command
failures as negative cases. These tests validate harness checks, not remote
join or CNI behavior. See the
[native report](../../../../../docs/validation/microk8s-prerequisite-results.json)
for coverage, resource limits and qualification boundaries.

The kubelet tests place the observed server line into a synthetic kubeconfig.
They resolve the active context and reject inactive local endpoints, comments,
URL suffixes and port prefixes. Context names and enclosing YAML are not native
observations. The [parser regression report](../../../../../docs/validation/microk8s-kubeconfig-results.json)
separates this updated validator's tests from the earlier native run.

`microk8s-1.34.9-kubelet.json` supplies actual control-plane and joined-worker
context names, cluster references and API endpoints from the
[two-VM native run](../../../../../docs/validation/microk8s-native-join-results.json).
Credentials, certificate data and user references are removed; remaining fields
are serialized as JSON. Per-config hashes identify the private source files.
These fixtures supplement the synthetic negative cases.
