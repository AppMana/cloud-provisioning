# Forked components

The product supports some configurations only with patched third-party
components. Each patch lives under `providers/`, pinned to an upstream
revision, with its own build and test instructions. Linux AWS workers, on the
AWS VPC CNI or on the site network over the tunnel, need none of them.

| Configuration | Fork | Upstream base | Enforced by the product |
| --- | --- | --- | --- |
| Windows workers on a k0s site using Konnectivity | [k0s `native-linux-egress.patch`](../providers/k0s/README.md#native-linux-default-routes-for-api-egress) | k0s v1.36.2+k0s.0, v1.36.4+k0s.1 | Yes: a Windows claim is refused while `konnectivity-agent-default-route-linux` is absent ([k0s API egress](k0s-windows-egress.md)) |
| Windows AWS workers bootstrapped through Secrets Manager | [CAPA `windows-bootstrap.patch`](../providers/capa/README.md) | CAPA v2.12.1 | No: CAPA's build is not visible to the controller; a stock CAPA cannot deliver the PowerShell bootstrap |
| Windows AWS workers on the AWS VPC CNI | [VPC resource controller `node-selector.patch`](../providers/aws-vpc-resource-controller/README.md), [VPC CNI plugins `host-routes.patch` and `endpoint-mtu.patch`](../providers/aws-vpc-cni-plugins/README.md) | resource controller `ca7792b`, plugins `8c8909e` | Yes: Windows VPC CNI claims are refused until `platforms.aws.vpcCNI.windows.ipamImage` names the patched controller, and each node's initializer refuses an image whose plugin is not the [pinned patched build](../controller/pkg/platform/aws/windowscni/cni-artifacts.json) |
| Windows tunnel-class workers on a Calico VXLAN site with a reduced MTU | [Calico `windows-mtu.patch`](../providers/calico/README.md) | Calico v3.32.0 | No |
| Windows GPU workers sharing a device without driver runtime mounts | [WDDM device plugin `runtime-mounts.patch`](../providers/windows-gpu-device-plugin/README.md) | `b21f59e` | No |
| k0s maintenance and Windows worker behaviour (exec streaming, static pods, Traefik, Konnectivity leases) | [k0s patches](../providers/k0s/README.md#preparing-a-complete-maintenance-runtime) | k0s v1.36.4+k0s.1 | No |

A configuration marked "No" fails at runtime without its fork rather than
being refused. Build each fork with the instructions beside its patch, pin the
result by digest, and qualify it before relying on the configuration.
