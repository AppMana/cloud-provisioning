# OKD worker Ignition exporter

This helper produces the distribution's complete worker Ignition configuration
without listening on a network socket. It invokes the Machine Config Operator's
server handler in-process and returns its response over authenticated Kubernetes
exec. It is a release-specific prototype; it does not yet implement remote
worker joining or an automatically reconciled exporter deployment.

## Release contract

- OKD: `4.21.0-okd-scos.11`.
- Machine Config Operator source: `1a0b9eee54cc8b45c91c5780bac2dbc32a6585ee`.
- Runtime image: the digest pinned in `Dockerfile`, matching this release's MCS.
- Output: Ignition 3.5, including the MCS appenders and bootstrap kubeconfig.

The build script requires a clean checkout at the exact commit and uses that
checkout's vendored dependencies. The nested `go.mod` excludes this helper from
the harness module; it must be built in the upstream MCO module.

```sh
git clone https://github.com/openshift/machine-config-operator.git .state/mco
git -C .state/mco checkout 1a0b9eee54cc8b45c91c5780bac2dbc32a6585ee
bash cluster/okd/exporter/build.sh .state/mco .state/okd-exporter
docker build -t cloud-provisioning/okd-exporter:scos-11 .state/okd-exporter
```

## Execution model

Run a dormant exporter pod in `openshift-machine-config-operator` using the
existing `machine-config-server` service account. Mount the native bootstrap
credential Secret at `/etc/mcs/bootstrap-token`, matching the release's MCS
DaemonSet. The helper uses the projected service-account credential for API
reads. It does not need the MCS TLS private key or a host mount.

Create a deny-ingress NetworkPolicy before the pod. Use no host network, host
ports or Service. The image entrypoint sleeps; invoke the helper only through
Kubernetes exec so the Ignition response does not enter pod logs. Restrict
`pods/exec` access to the trusted bootstrap controller/operator. Keep output in
private files or bootstrap Secrets and avoid logging response bodies.

```sh
umask 077
kubectl -n openshift-machine-config-operator exec cldt-ignition-exporter -- \
  /usr/local/bin/ignition-exporter \
  --api-url=https://api-int.okd.cldt.test:6443 \
  --payload-version=4.21.0-okd-scos.11 > worker.ign
```

Supply the cluster's actual API URL and observed payload version. The helper has
a hard export deadline. The output contains bootstrap credentials and must not
be committed or served publicly.

The verified release output contains 65 files and 29 units with no remote
Ignition merge reference. The release's MCS firewall rejects new TCP connections
to ports 22623/22624, including inside an ordinary exporter pod. In-process
handler execution needs neither those ports nor firewall changes.

## Remote bootstrap integration

A production join provider must verify the cluster payload against the exporter
image/source pin, obtain the complete worker configuration, and add the product's
WireGuard files and host units without overwriting MCO-owned content. SCOS must
consume the result as Ignition on first boot. Cloud-init interpretation is not a
substitute for this path.

The remaining integration requirements are API TLS-name preservation over the
tunnel, host-service ordering before kubelet, CSR handling, providerID association,
OVN remote-node routing, and lifecycle/matrix validation. The configuration is
larger than EC2 user-data capacity; AWS needs a private supported bootstrap
storage transport. CAPA's Ignition schema and accepted wrapper versions must be
checked against the selected payload. Export success alone does not establish
these properties.
