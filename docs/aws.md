# AWS provisioning

Cluster API Provider AWS (CAPA) creates and deletes EC2 workers for an existing
on-premises Kubernetes cluster. The product renders the distribution's join
configuration and WireGuard identity; CAPA owns the AWS instance lifecycle.
The on-premises tunnel endpoints initiate UDP connections to each worker's
public address. The control-plane API remains private.

This configuration targets CAPA **v2.12.1** with pre-existing infrastructure and
an externally managed control plane. It does not grant CAPA permission to create
VPCs, load balancers, NAT gateways, EKS clusters or IAM roles. A different CAPA
infrastructure mode needs a different policy.

For graphics and short-lived GPU workers, see [GPU workers and reusable
images](gpu-workers.md). GPU instance selection also requires a compatible,
licensed image and an updated AMI allowlist and any instance-type restrictions
in the IAM policy.
The existing Ubuntu preparation helper and CPU integration results do not
establish Windows or GPU readiness.

## Add an AWS worker

For a separate host that runs the full QEMU test harness, see
[EC2 runner capacity](#ec2-runner-capacity). That host is infrastructure for the
test campaign; it is not a worker claim in the cluster being tested.

**Apply a template and a claim to Kubernetes. The claim creates the instance.**
The template is a reusable machine configuration; one claim requests one EC2
worker. A second claim can reference the same template.

The product reads `AWSMachineTemplate.spec.template.spec` and copies it into a
new `AWSMachine`. It also creates a CAPI `Machine`. CAPA launches EC2 using that
infrastructure object, and the product supplies native distribution bootstrap
and WireGuard configuration.

### Where the objects go

Apply them to the API where this project's controller and the CAPI/CAPA
controllers run. In the validated setup those controllers run inside the
on-premises cluster. The template, claim and referenced CAPI `Cluster` must all
be in **the same namespace**, normally `cloud-provisioning`.

The filename and repository directory are your choice. The product watches
Kubernetes objects, not a folder on disk. Use `kubectl apply`, or put the files
under a path reconciled by your Flux/Argo CD configuration. A file committed
outside that configured path will not provision anything.

### One-time setup

Before applying worker claims, configure:

- CAPI, CAPA, cert-manager and the product chart, including
  `providerManagerNamespace=capa-system`.
- A CAPI `Cluster` and its externally managed `AWSCluster`, pointing at the
  existing VPC/subnet and an `AWSClusterStaticIdentity` with usable credentials.
- The imported site's control-plane contract and
  `<cluster-name>-kubeconfig` connection Secret, accessible to CAPI.
- A prepared AMI, worker instance profile and scoped IAM policies.
- A matching `joinProvider`, reachable first-boot dialer download with its
  SHA-256, and a remotely pullable dialer image or configured host-binary
  adoption image. These chart settings are separate from the machine template.

The [validated VM/AWS setup](#running-the-aws-worker-matrix) supplies the import
contract through `cmd/importsite` and the harness's `ImportedControlPlane` CRD.
That import mechanism is currently harness tooling, not an import controller
installed by the product chart. The [cluster-level example](../examples/aws.yaml)
shows the alternative externally managed Cluster shape; it does not create a
connection Secret or reproduce the validated import contract by itself.
A ready-looking Cluster or an EC2 launch alone is not proof of Node association.

### Create the first worker

Edit the [worker-only manifest](../examples/aws-worker.yaml):

| Field | Value to supply |
| --- | --- |
| Template `metadata.namespace` | Namespace of the existing CAPI Cluster |
| Template `metadata.name` | Reusable profile name, e.g. `public-worker` |
| `spec.template.spec` | CAPA machine settings: instance type, approved AMI, subnet, security group, profile, ownership tag and root volume |
| Claim `metadata.namespace` | Same namespace as the template and Cluster |
| Claim `metadata.name` | Unique worker request, e.g. `public-worker-1` |
| Claim `spec.infrastructureRef.name` | Template name |
| Claim `spec.clusterName` | Existing CAPI Cluster name, e.g. `my-cluster` |

Replace the example AWS identifiers and the ownership tag with values matching
your rendered IAM policies. Then apply the file using the intended kubeconfig
context:

~~~sh
kubectl --context onprem-admin apply -f examples/aws-worker.yaml
kubectl --context onprem-admin -n cloud-provisioning get provisionednodeclaim,machine,awsmachine
kubectl --context onprem-admin -n cloud-provisioning get machine public-worker-1 -w
~~~

Check that the Machine becomes `Running` and gains `status.nodeRef.name`.
Check that the referenced Kubernetes Node is `Ready` and that its
`spec.providerID` matches the Machine's AWS provider ID. EC2's hostname can
differ from the claim name; use `nodeRef` rather than assuming they match.

### Add another worker with the same configuration

Keep the template and create another claim:

~~~yaml
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: public-worker-2
  namespace: cloud-provisioning
spec:
  clusterName: my-cluster
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: AWSMachineTemplate
    name: public-worker
~~~

Save that as `worker-2.yaml` and apply it with the same kubeconfig context.
Each claim gets its own Machine, AWSMachine, instance and tunnel identity.

### Remove or replace a worker

For a worker using a Windows gateway attachment, the gateway controller installs
per-lease pre-drain and pre-terminate hooks on the worker and gateway. Deleting
the claim requests Machine deletion; the hooks hold infrastructure while the
controller withdraws the attachment and releases its owned resources. Shared
gateway leases retain their own hooks. Deploy the controller and chart RBAC
together: automatic withdrawal requires ConfigMap deletion in the controller's
namespace. See [gateway removal ordering](windows-gateway.md#enable-the-aws-attachment-controller).

For a deployment without these paired hooks, first delete the attachment request
and wait for withdrawal and resource release before deleting the claim.

Delete its **claim**, keeping the shared template:

~~~sh
kubectl --context onprem-admin -n cloud-provisioning delete provisionednodeclaim public-worker-1 --wait=true --timeout=10m
kubectl --context onprem-admin -n cloud-provisioning get machine,awsmachine
~~~

The product and CAPI/CAPA reconcile node and instance removal. If deletion
times out, inspect the claim, Machine and AWSMachine conditions; leave their
finalizers in place so cleanup can finish. With GitOps, remove the claim from
the reconciled source too, or it may be created again.

The claim controller requests Machine deletion and waits for CAPI to finish
before collecting orphaned infrastructure objects. It does not directly delete
an AWSMachine while its Machine still exists, so CAPI can enforce its deletion
hooks and drain policy. Imported control planes must publish
`status.externalManagedControlPlane: true`: their control-plane nodes exist
outside CAPI's Machine inventory. Without that field, CAPI can skip draining
and pre-drain hooks because it sees no control-plane Machines.

Editing a template or a claim does **not** resize or roll an existing instance.
The controller leaves an existing AWSMachine specification untouched. For a
different configuration, create a new template name and replace the claim after
its old Machine and Node have disappeared. Alternatively, add a differently
named claim first, verify its worker, then remove the old claim. Deleting a
template is not the operation for removing the workers created from it.

## Required AWS resources

Prepare a VPC, a public subnet with an Internet Gateway route, a security group,
a distribution-compatible AMI, and an EC2 instance profile. Each worker needs
one ENI. Permit inbound UDP 51820 for WireGuard and any ports explicitly required
by workloads placed on the worker. Allow outbound traffic for bootstrap downloads,
AWS service access and workload traffic. No inbound SSH rule is required.

CAPA secure cloud-init bootstrap requires the **AWS CLI already installed in
the AMI**, along with Bash and cloud-init. Installing it in the product
user-data is too late: CAPA needs it to retrieve that user-data from Secrets
Manager. The image also needs CAPA-compatible cloud-init include handling:
`ERROR_ON_USER_DATA_FAILURE = False`, as configured by the upstream
[image-builder provider role](https://github.com/kubernetes-sigs/image-builder/blob/96bd49f71859ff7863f08ba4dc3321c560f8802b/images/capi/ansible/roles/providers/tasks/main.yml).
CAPA's boothook writes `/etc/secret-userdata.txt` after the first include parse;
strict missing-include handling prevents the boothook from running. Build this
into the image rather than altering a worker after its failed boot. Prepare and
validate the AMI before creating worker claims.

The validated k0s image uses Ubuntu 22.04, cloud-init 26.1 and direct file output:

```yaml
# /etc/cloud/cloud.cfg.d/99-capa-output.cfg
output: {all: '>> /var/log/cloud-init-output.log'}
```

CAPA restarts cloud-init after retrieving private user data. A logging pipe to
`tee` can close during that restart and raise `BrokenPipeError` in cloud-init's
termination handler, causing worker configuration to be skipped. The image
helper installs the direct-output setting, cleans cloud-init state, syncs writes,
and waits for the builder to stop before snapshotting. Require a fresh CAPA
instance to join and pass pod networking checks before accepting an image;
successful SSM access or bootstrap-secret retrieval alone is insufficient.

The AWS integration uses **k0s v1.34.1+k0s.0** and its bundled
**Calico v3.29.6-0**, with the distribution's default VXLAN configuration.
Five single-NIC site VMs and two single-ENI EC2 workers pass all **140** checks
for pod traffic, Services, 1 MiB transfers, DNS and external access.
The full AWS validation passes **1,184 checks across nine gates**: two independent
delete-and-readd cycles, survivor connectivity, and all four endpoint placements.
Both replacements require new EC2 instance IDs and Kubernetes Node UIDs.
See the [AWS k0s/Calico evidence](validation/aws-k0s-calico.md) for scope and coverage.

For k0s, k3s and RKE2, use a compatible cloud-init image with systemd and
WireGuard support. MicroK8s additionally needs snapd and Python 3; its current
pattern installs snapd using apt when absent. kubeadm requires an image containing
kubeadm, kubelet and a container runtime at the selected Kubernetes version.
OKD requires a release-compatible SCOS image and Ignition; its remote provisioning
path is not yet validated. See [join patterns](../join-patterns/README.md).

## IAM roles and policy files

Use separate identities for provisioning and guest execution. The JSON files
below are parameterized templates; render them before attaching them to a role.
The `cloud-provisioning-test` ownership tag scopes this isolated integration
configuration. Every AWSMachine must carry the selected tag value.

| Role | Policies | Purpose |
| --- | --- | --- |
| CAPA controller | [capa.json](iam/capa.json), [capa-trust.json](iam/capa-trust.json) | Discover EC2 resources; launch the approved AMI in the approved subnet/security group; manage tagged instances and bootstrap Secrets Manager entries; pass only the worker role |
| EC2 worker | [worker-bootstrap.json](iam/worker-bootstrap.json), [worker-trust.json](iam/worker-trust.json) | Read and delete this run's CAPA bootstrap secrets |
| Binary publisher | [assets.json](iam/assets.json) | Create a private test bucket and publish expiring downloads; keep this role separate from CAPA |
| Windows image publisher | [publisher.json](iam/publisher.json) | Manage the run’s private Windows component ECR repositories and issue repository-scoped pull sessions; separate from CAPA and workers |
| Harness operator | [harness.json](iam/harness.json) | Inspect instances, issue EC2 power operations, run SSM commands on tagged instances, and stage private command input |
| Separate VM runner operator | [runner.json](iam/runner.json), [capa-trust.json](iam/capa-trust.json) | Launch one approved instance type and base AMI in approved network resources; terminate owned runners; use SSM and an owned artifact prefix |
| Gateway route adapter (experimental) | [gateway-routes.json](iam/gateway-routes.json) | Observe route tables/interfaces and create/delete return routes in one explicitly named, run-tagged route table |
| Gateway source/destination-check adapter (experimental) | [gateway-source-check.json](iam/gateway-source-check.json) | Observe interfaces and change only SourceDestCheck on one explicitly named, run-tagged gateway ENI |
| Gateway UDP ingress adapter (experimental) | [gateway-ingress.json](iam/gateway-ingress.json) | Observe groups/rules, add tagged rules and revoke ingress in one named, run-tagged security group |

The gateway route policy uses `AWS_REGION`, `AWS_ACCOUNT_ID`, `ROUTE_TABLE_ID`
and `RUN_ID` when rendered with `docs/iam/render.py`. Attach the rendered policy
to the identity running the route adapter, separately from worker bootstrap.
EC2 scopes CreateRoute/DeleteRoute authorization to the route table; individual
route ownership is therefore maintained in the adapter's lease journal. See the
[EC2 authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ec2.html).
The adapter refuses existing routes without matching durable ownership and
checks the table's account, VPC and ownership tag before changes. It supports
only explicit IPv4 host routes and refuses to override the VPC local range.
This policy covers the route component only. Source/destination-check and
security-group changes use the separate policies below. Enable the
[attachment controller](windows-gateway.md#enable-the-aws-attachment-controller)
to reconcile the components together.

The reusable route check creates one return route for two CAPI worker leases,
verifies that the first release preserves it, then verifies that the last
release restores the original route table:

```sh
cd harness/e2e
go run ./cmd/awsroutecheck --work-dir .state/aws/<run> \
  --plans <observed-gateway-plan-results.json> \
  --api-server https://<isolated-site-api>:6443 \
  --destination <observed-site-host>/32 \
  --output-dir .state/aws/<run>/route-check
```

Store the short-lived, route-policy-scoped STS response in the run's private
`gateway-session.json`. The check reloads those credentials for each AWS call.
It resolves the observed Machine and Node UIDs through the bastion and checks
that both plans name the same gateway and include the requested return host.
It locks the run against another simultaneous route check. Use a new output
directory for each check; its intent journal and lease inputs support recovery:

```sh
go run ./cmd/awsroutecheck --recover --work-dir .state/aws/<run> \
  --output-dir .state/aws/<run>/route-check
```

Recovery releases only the saved leases, checks the current run scope and
requires the target route to be absent before reporting success. It does not
need the worker Node or gateway instance to survive. An ENI reassigned to a
different instance, a changed route target or lost ownership prevents deletion.
The [route validation evidence](validation/aws-gateway-route-results.json)
covers this provider component; it does not establish complete Windows CNI
networking or automatic gateway attachment.

The gateway source/destination-check policy uses `AWS_REGION`, `AWS_ACCOUNT_ID`,
`GATEWAY_ENI_ID` and `RUN_ID`. Render it with `docs/iam/render.py` and attach it to
the gateway reconciliation identity, separately from the worker role:

```sh
python3 docs/iam/render.py docs/iam/gateway-source-check.json iam-values.json > gateway-source-check.json
aws iam put-role-policy --role-name <gateway-controller-role> \
  --policy-name gateway-source-check --policy-document file://gateway-source-check.json
```

The mutation permission names the exact ENI ARN and requires its
`cloud-provisioning-test` ownership tag. The
`ec2:Attribute/SourceDestCheck` condition allows the Boolean setting to be
disabled and restored; it does not grant changes to other interface attributes.
This uses AWS's [attribute-specific IAM conditions](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-policies-for-amazon-ec2.html).
Real EC2 DryRun calls allowed both setting values and denied a description
change with the same temporary session.

The adapter records the original setting before modifying EC2. Worker leases
share the disabled setting; the final release restores it only if it was
originally enabled. An initially disabled setting remains disabled. Its
ConfigMap journal retains a finalizer until restoration completes, and cleanup
refuses an ENI reassigned to another instance. Run one elected writer per
provider scope and retain the journal through worker deletion.

The [source-check validation evidence](validation/aws-gateway-source-check-results.json)
records two live CAPI worker leases, original ENI restoration and native
coverage. The component is registered when the opt-in attachment controller is
enabled; attaching the policy alone does not configure a gateway.

The gateway ingress policy uses `AWS_REGION`, `AWS_ACCOUNT_ID`,
`SECURITY_GROUP_ID` and `RUN_ID`. Render `docs/iam/gateway-ingress.json` with
`docs/iam/render.py` and attach it to the gateway reconciliation role. It grants
rule creation and ingress revocation in the named, tagged security group;
`CreateTags` is limited to rule creation and the two ownership tag keys.

EC2 authorizes `RevokeSecurityGroupIngress` against the security group, not an
individual rule ARN. The policy therefore cannot isolate revocation to the
adapter's rules within that group. The adapter persists a random ownership tag,
checks the observed permission and recorded rule ID, and revokes only that ID
after the final lease release. It rejects an existing matching permission
without ownership. See the [EC2 authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ec2.html).
Do not grant this role to worker pods. The adapter validates an explicit IPv4
host source and UDP port; these packet fields are application constraints,
not restrictions enforced by this IAM policy. The
[native ingress validation](validation/aws-gateway-ingress-results.json) records
one tagged create, shared-lease retention and one exact-ID revoke using a
temporary session restricted by this policy. All original rules were restored.

For the gateway controller's guest observations, attach
[gateway-observation.json](iam/gateway-observation.json) to its AWS role after
substituting the region, account and ownership tag value. This policy permits
ENI lookup, invocation status lookup and shell/PowerShell documents on tagged
instances. IAM permits arbitrary script content through those AWS documents;
the controller's guest reader restricts calls to fixed HNS and Linux forwarding
probes. This policy grants no power operations or S3 transfer access. Use the
separate gateway resource policies for route, ingress and source-check changes.
The opt-in [gateway runtime](windows-gateway.md#enable-the-aws-attachment-controller)
loads credentials separately from its placement scope. The
[combined CAPI worker cycle](validation/windows-peer-membership-both-results.json)
validated attachment readiness and cleanup while preserving other worker
leases. Gateway deletion and the full failure matrix remain separate gates.

Attach AWS's managed `AmazonSSMManagedInstanceCore` policy to the worker role
when SSM diagnostics are required. The SSM agent must also be installed and able
to reach its regional endpoints. That managed policy does not authorize CAPA
provisioning or grant the harness permission to send commands.

The Windows secure-bootstrap extension requires EC2Launch v2 and detaches its
one-shot userdata script. EC2Launch normally starts SSM after inline userdata;
detachment allows management to start while bootstrap is still running. It does
not make SSM independent of guest networking or prove a successful join. Check
the Machine/Node association and guest receipts separately. See the
[bootstrap wrapper contract](../providers/capa/README.md#bootstrap-contract)
and [AWS execution-order documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2launch-v2-settings.html).

CAPA uses Secrets Manager for cloud-init bootstrap by default. Keep
`cloudInit.insecureSkipSecretsManager: false`. The worker bootstrap policy grants
`GetSecretValue` and `DeleteSecret` only for CAPA's
`aws.cluster.x-k8s.io/` secret prefix with this run's ownership tag. The CAPA
policy grants creation/tagging and deletion of those secrets. These policies
assume the AWS-managed Secrets Manager encryption key; a customer-managed KMS
key requires additional key policy and IAM permissions.

CAPA retries deletion after the guest has already deleted its bootstrap secret.
The controller's delete condition uses `StringEqualsIfExists` so a missing tag
does not turn that idempotent cleanup into `AccessDenied`. A present ownership
tag must match the run. This also permits deletion of **untagged** secrets under
the CAPA prefix in the selected account and region; reserve that prefix for CAPA
and consistently tag its secrets. Read access still requires the matching tag.
See [IAM condition semantics](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_condition_operators.html).

### Render and attach

Create a local `iam-values.json` with actual resource identifiers. It contains
configuration, not access keys:

```json
{
  "AWS_ACCOUNT_ID": "123456789012",
  "AWS_REGION": "us-west-2",
  "RUN_ID": "cldt-example",
  "AMI_ID": "ami-0123456789abcdef0",
  "SUBNET_ID": "subnet-0123456789abcdef0",
  "SECURITY_GROUP_ID": "sg-0123456789abcdef0",
  "NODE_ROLE_NAME": "cldt-example-node",
  "INSTANCE_PROFILE_NAME": "cldt-example-node",
  "SOURCE_PRINCIPAL_ARN": "arn:aws:iam::123456789012:role/test-runner",
  "ASSET_BUCKET": "cldt-example-123456789012-assets"
}
```

From the repository root:

```sh
mkdir -p harness/e2e/.state/aws/iam
python3 docs/iam/render.py docs/iam/capa.json iam-values.json \
  > harness/e2e/.state/aws/iam/capa.json
python3 docs/iam/render.py docs/iam/capa-trust.json iam-values.json \
  > harness/e2e/.state/aws/iam/capa-trust.json
aws iam create-role --role-name cldt-example-capa \
  --assume-role-policy-document file://harness/e2e/.state/aws/iam/capa-trust.json
aws iam put-role-policy --role-name cldt-example-capa --policy-name provisioning \
  --policy-document file://harness/e2e/.state/aws/iam/capa.json
```

Create the worker role and its instance profile:

```sh
python3 docs/iam/render.py docs/iam/worker-trust.json iam-values.json \
  > harness/e2e/.state/aws/iam/worker-trust.json
python3 docs/iam/render.py docs/iam/worker-bootstrap.json iam-values.json \
  > harness/e2e/.state/aws/iam/worker-bootstrap.json
aws iam create-role --role-name cldt-example-node \
  --assume-role-policy-document file://harness/e2e/.state/aws/iam/worker-trust.json
aws iam put-role-policy --role-name cldt-example-node --policy-name bootstrap \
  --policy-document file://harness/e2e/.state/aws/iam/worker-bootstrap.json
aws iam create-instance-profile --instance-profile-name cldt-example-node
aws iam add-role-to-instance-profile --instance-profile-name cldt-example-node \
  --role-name cldt-example-node
# Optional guest diagnostics:
aws iam attach-role-policy --role-name cldt-example-node \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
```

Put the profile name in `AWSMachineTemplate.spec.template.spec.iamInstanceProfile`.
Attach the harness policy to the test runner role, not to the EC2 worker role.
The source principal must separately be permitted to call `sts:AssumeRole` on
the CAPA role; the trust policy identifies who may assume it.

The CAPA policy separates existing launch resources from resources created by
`RunInstances`: request-tag conditions cannot authorize an existing AMI or
subnet. CAPA tags the new instance, volume and ENI at launch. This configuration
uses `additionalSecurityGroups` for the externally managed AWSCluster; CAPA
does not use `securityGroupOverrides` in that mode. It uses on-demand instances,
an automatically created primary ENI, no EC2 key pair,
and no extra volumes or security groups outside the rendered policy.

The four permission templates pass IAM Access Analyzer validation with no
findings. The isolated CAPA test also exercised EC2 launch and termination,
private bootstrap retrieval, and SSM execution. That policy-only artifact is separate from the k0s/Calico
network matrix described above. The
[policy validation artifact](validation/aws-policy-results.json) records template
hashes, findings, and IAM simulation of matching, different and absent ownership
tags for bootstrap-secret deletion.

These policy templates require live CAPA validation for your selected image and
infrastructure settings. Successful IAM role creation or STS authentication alone
does not prove that CAPA can provision and delete a joined worker.

## Supplying CAPA credentials

Prefer short-lived role sessions to new long-lived access keys. An
`AWSClusterStaticIdentity` resolves its credential Secret in **`capa-system`**,
the CAPA controller's namespace. The Secret keys are `AccessKeyID`,
`SecretAccessKey`, and, for STS credentials, `SessionToken`. Restrict
`allowedNamespaces` to the namespace containing the intended AWSCluster.

Set the product chart's `providerManagerNamespace=capa-system` as well. Its
infrastructure validator checks that the referenced credential Secret exists;
this value installs the namespace-scoped read Role and binding it needs.
Without that Role, validation fails with `Forbidden` before publishing bootstrap
data, even when CAPA itself can read its credentials.

Keep the source account credentials out of Kubernetes. Publish only the derived
role session and renew it before expiration; an expired session prevents CAPA
from completing creates and deletes. Do not put credential-bearing manifests,
user data, or session JSON in Git or diagnostic output.

The [AWS manifests](../examples/aws.yaml) show the object relationships. Replace
all example identifiers and configure the AWSMachine template with:

```yaml
additionalTags:
  cloud-provisioning-test: cldt-example
iamInstanceProfile: cldt-example-node
sshKeyName: ""
publicIP: true
cloudInit:
  insecureSkipSecretsManager: false
additionalSecurityGroups:
  - id: sg-0123456789abcdef0
```

An externally managed AWSCluster prevents CAPA from creating a cloud control
plane. CAPI Node association additionally requires the imported cluster's
kubeconfig and control-plane contract; an external-management annotation alone
is not proof of `Machine.status.nodeRef` association. The VM harness supplies
that contract through its ImportedControlPlane provider.

## Test environment setup and cleanup

The local test setup uses
`harness/e2e/aws/bootstrap.py` and records each created resource immediately in a
private `resources.json`. It creates a separate tagged VPC, subnet, Internet
Gateway, route table, security group, EC2 role/profile and CAPA role, then writes
a one-hour STS session. It resolves a Canonical Ubuntu Noble **base** AMI and renders
the same CAPA and worker-bootstrap policies linked above, saving the resolved
values and policy documents alongside the resource inventory. The stock base
image must be prepared with AWS CLI and compatible cloud-init include handling
before secure-bootstrap worker tests; update
`amiID`, `AMI_ID` and the attached CAPA policy to the resulting AMI.

For this workspace, run from `~/Documents/appmana/appmana-cluster/src` in Bash:

```sh
umask 077
source ./source-me.sh > /tmp/cldt-aws-source.log 2>&1
python3 ~/Documents/cloud-provisioning/harness/e2e/aws/bootstrap.py \
  --work-dir ~/Documents/cloud-provisioning/harness/e2e/.state/aws/example \
  --run-id cldt-example
```

`bootstrap.py --base-ami-id` selects an explicit Canonical x86_64 base image
instead of the default Noble lookup. Keep the resolved ID in the run inventory.
Image discovery runs before creating cloud infrastructure, so an invalid image
selection does not leave a VPC and IAM roles behind.

The setup identity needs these actions in the test account:

- Discovery: `sts:GetCallerIdentity`, `ec2:DescribeAvailabilityZones` and
  `ec2:DescribeVpcs`/`DescribeInstances` for cleanup and verification, plus
  `iam:GetRole`, `iam:GetInstanceProfile` and `s3:GetBucketTagging` for cleanup
  read-back checks. Image verification also uses `ec2:DescribeImages` and
  `ec2:DescribeSnapshots`.
- Network setup: `ec2:CreateVpc`, `ModifyVpcAttribute`, `CreateSubnet`,
  `ModifySubnetAttribute`, `CreateInternetGateway`, `AttachInternetGateway`,
  `CreateRouteTable`, `AssociateRouteTable`, `CreateRoute`,
  `CreateSecurityGroup`, `AuthorizeSecurityGroupIngress`, and `CreateTags`.
- IAM setup: `iam:CreateRole`, `TagRole`, `AttachRolePolicy`, `PutRolePolicy`,
  `CreateInstanceProfile`, `TagInstanceProfile`, `AddRoleToInstanceProfile`,
  `sts:AssumeRole` for the test role, and `sts:GetFederationToken` for the
  scoped harness session when running with IAM-user setup credentials.
- Cleanup: `ec2:TerminateInstances`, `DeleteSecurityGroup`,
  `DisassociateRouteTable`, `DeleteRouteTable`, `DeleteSubnet`,
  `DetachInternetGateway`, `DeleteInternetGateway`, `DeleteVpc`,
  `iam:RemoveRoleFromInstanceProfile`, `DeleteInstanceProfile`,
  `DetachRolePolicy`, `DeleteRolePolicy`, and `DeleteRole`.

Image preparation additionally needs `ec2:CreateImage`, `CreateTags`,
`DescribeImages`, `DescribeSnapshots`, `DeregisterImage` and `DeleteSnapshot`.
The disposable image builder also needs `ec2:RunInstances`, `StopInstances`,
`DescribeInstances`, `iam:PassRole` for the worker role, and SSM command access.
For the pinned Windows GRID image layer, attach
[the exact-object download policy](iam/windows-grid-download.json) to the
principal that fetches the installer during image preparation. A completed
GPU worker image does not require that permission at startup. Keep the package
and resulting AMI within [AWS's GRID license scope](gpu-workers.md#cloud-and-windows-boundaries).
Use [image-prepare.sh](../harness/e2e/aws/image-prepare.sh) only on a disposable
image-builder instance: it installs digest-pinned AWS CLI and clears cloud-init
state for imaging. It does not join a cluster. Tag the AMI and snapshots with the
run ownership tag and record `preparedAMIID` in the resource inventory; cleanup
checks ownership before deregistering that image and deleting its snapshots.

The [image helper](../harness/e2e/aws/bake.py) separates preparation from worker
provisioning. From the repository root, with setup credentials:

```sh
python3 harness/e2e/aws/bake.py start --work-dir harness/e2e/.state/aws/example
# After the builder completes preparation and SSM becomes available:
python3 harness/e2e/aws/bake.py capture --work-dir harness/e2e/.state/aws/example
# After the candidate AMI reaches available:
python3 harness/e2e/aws/bake.py promote --work-dir harness/e2e/.state/aws/example
```

Capture verifies preparation through SSM, cleans cloud-init state, syncs writes,
and stops the builder before taking its snapshot. Snapshotting a running builder
without flushing writes can preserve a new file's directory entry with empty
contents. Promotion updates the scoped CAPA policy and worker AMI together, then
terminates the builder. Previous candidates remain in the cleanup inventory.
Capture records the SSM command ID with the builder ID and verification-command
hash. If host observation times out, rerun the same `capture` command: it observes
the original guest operation. A completed successful verification is retained
across an interruption while stopping the builder. `--replace-candidate` starts
a new verification for an updated builder and retains the previous receipt.
Run only one inventory writer at a time.

A lost submission response leaves an intent without a command ID. Capture stops
in that case, or when a legacy command lacks its instance/recipe binding. Inspect
SSM command history for the recorded builder and reconcile the original command
before resuming; do not clear that intent while its outcome is unknown. A failed
guest command is retained as a failure and cannot produce an image. These guards
cover SSM preparation; a lost `CreateImage` response still requires reconciling
the run's AMIs before rerunning capture.

These preparation checks do not replace a fresh CAPA worker bootstrap test.
The claim helper rejects a run still selecting its unprepared Ubuntu base AMI;
prepare and promote the Linux candidate before submitting that claim. Windows
claims use their separately captured images and do not depend on Linux image
preparation.

To test an additional Linux image without replacing the run default, authorize
its exact private AMI and select it explicitly in the claim:

```sh
python3 harness/e2e/aws/bake.py authorize --work-dir <run-directory> --image-id <candidate-ami>
python3 harness/e2e/aws/claim.py --work-dir <run-directory> \
  --api-server https://<isolated-site-api>:6443 --name <new-claim> \
  --linux-image-id <candidate-ami> --instance-type <worker-type>
```

Authorization checks the image's account, run tag, availability and Linux OS,
then adds only that image ARN to the live CAPA launch policy. It preserves the
existing statements and default AMI. The claim helper requires explicit Linux
selections to appear in `authorizedLinuxAMIs` and records the choice in an
`AWSMachineTemplate`. CPU/GPU instance shape is selected independently of the
image; the image still needs drivers appropriate to that shape. Authorization
is permission to test, not evidence of image qualification.

Pass `-expected-image <candidate-ami>` to `cmd/awsrow` for that claim. The row
checks the run's authorization inventory and the actual EC2 AMI and physical
NIC count. Its `bindings.json` records the observed image ID. Both subsequent
Windows authorization and Linux promotion retain and revalidate additional
Linux image grants. Use `observe/linux_image_reuse.py` to validate the saved
candidate and native observation, including the baked-executable hash, receipt
and pre-launch file timestamp. It checks the Kubernetes server version separately
from the containerd client. The [native candidate report](validation/linux-k0s-baked-capi-results.json)
records image-reuse and lifecycle qualification; a diagnostic survivor result
does not qualify a release.

In a mixed Linux/Windows test run, Linux promotion retains the previously
authorized Windows image IDs. It rechecks their availability, private ownership
and run tags before updating the CAPA role. This lets Linux and Windows machine
templates continue to share the role without granting access to arbitrary AMIs.

For a Linux GPU candidate, `bake.py start --instance-type g4dn.xlarge
--prepare-script <recipe.sh> --verify-script <acceptance.sh>` composes additional
preparation with the shared AWS bootstrap prerequisites. Supply both scripts.
The helper checks preparation and acceptance scripts with `bash -n`, rejecting
parser warnings as well as nonzero exits before launching an instance. This
catches unterminated heredocs, for which Bash can return zero. It then saves
their contents and hashes in the private run directory; capture checks the saved acceptance script's hash and runs it before cleaning the image.
Capture does not reinstall the build tooling. A required driver reboot must
complete before capture. The scripts run as root on the disposable builder;
keep them free of credentials and cluster identity. This interface currently
accepts Linux scripts only.
If an incremental build adds a runtime cache after its original preparation,
pass `capture --capture-check <cache-acceptance.sh>` on the first capture attempt
(with `--replace-candidate` when retaining an earlier candidate). This appends
acceptance to the original build verifier. The helper saves a content-addressed
copy and binds it to the builder before submitting SSM work. Resume using the
same capture command; the saved check is used even without repeating the option.
A different check, a modified saved copy, or adding a check after capture has
begun is rejected. Capture checks must inspect existing artifacts rather than
download or install them.
Recipe options apply to the new build. If omitted, the builder uses only the
shared AWS bootstrap preparation; a previous build's GPU or other acceptance
script is not inherited. Completed-builder history retains the prior recipe
hashes, and existing script files are retained as evidence until a later recipe
explicitly replaces them.
For a layered build, add `--base-image-id <owned-linux-ami>` to `start`.
The helper validates the private parent against the account/run, requires an
x86_64 HVM image with one EBS root disk, and preserves its root device name and
minimum volume size. It records the parent without changing the default worker
AMI or granting CAPA permission to launch it. See the
[shared image composition workflow](../images/README.md) for GPU layers and
the separate reboot, capture and fresh-worker acceptance requirements.

For Linux runtime caches, use the [independent native cache check](../images/README.md)
before capture. It opens the prepared store without pulling or importing images
and requires the selected references to remain complete and unpacked across a
runtime restart. Run fresh-worker cache observations before `awsrow` imports test
images; those imports would hide missing baked entries.

SSM file transfers can leave signed URLs in historical guest command scripts.
Before capture, obtain successful `GetCommandInvocation` receipts for the exact
instance and transfer command IDs. The AWS-specific
[`linux_transfer_cleanup.py`](../harness/e2e/aws/linux_transfer_cleanup.py) helper
accepts an explicit manifest of those receipts and their script paths. It rejects
symlinks, mismatched identities and scripts referenced by live guest processes,
and returns hashes of the scripts it removes. It preserves agent identity and
other command history. This is a separate preparation step, not an automatic
part of `bake.py`; archive its receipt privately and check for remaining transfer
residue before capture. Do not stage the cleanup manifest through another signed
transfer after that final check.
Inspect the SSM agent logs as well as its document directory. Cleaning completed
scripts does not remove URLs already written to logs. On the disposable builder,
clear affected logs after retaining the required diagnostic evidence and record
their hashes; preserve the active agent's log inode and recheck after cleanup.

`claim.py --instance-type <type> --root-volume-gib <size>` selects the actual
CAPA worker shape independently of the builder. The prepared AMI must support
that machine's architecture and GPU family. The example CAPA IAM policy scopes
AMI, network and ownership tags but does not currently restrict instance types;
add an `ec2:InstanceType` condition if your deployment requires a type allowlist.

For captured Windows images, `windows_image.py --phase status` records the AMI's
root EBS volume size as `rootVolumeGiB`. The claim helper defaults to at least
that size and rejects a smaller explicit `--root-volume-gib` before applying
cluster objects. Refresh image status after capture, especially when adding
pre-unpacked full Windows Server workload images to a GPU cache. Existing image
records without this field retain the flavor minimum until status is refreshed.

Windows startup-cache qualification must include runtime sandbox images such as
`pause`, alongside the CNI, kube-proxy, tunnel publisher, device plugin and first
application. Check both registered targets and unpacked snapshots on a fresh VM,
then correlate cache-hit events with that VM's Pod and Node UIDs. A passing image
cache check does not establish loss-free networking or a startup-time guarantee.

EC2 Fast Launch is an optional, separately qualified boot optimization. It
pre-provisions Windows initialization into snapshots, replenishes consumed
snapshots and falls back to normal boot when none are available. EC2 and storage
charges still apply. Keep a bounded snapshot pool and measure launch-to-first-GPU
workload against the same AMI without Fast Launch before adopting it. It has not
been validated with this project's GPU images.
[AWS Fast Launch overview](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/win-ami-config-fast-launch.html).

Use a dedicated Fast Launch EC2 launch template with a numbered version for the
pre-provisioning network and IMDSv2 settings. AWS documents `t3.xlarge` for this
template and prohibits user data and Spot configuration in it. This template is
separate from the CAPA `AWSMachineTemplate`; cluster join data belongs to the
actual worker launch. Keep Fast Launch setup permissions on the image operator
identity and its service-linked role, separate from the CAPA worker provisioning
policy. Disabling Fast Launch removes its pre-provisioned snapshots.
[AWS configuration and permission checks](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/win-fast-launch-configure.html).

If an existing image builder needs a larger disk, attach the optional
[image-builder-volume.json](iam/image-builder-volume.json) policy to the image
builder's operator identity. Render `AWS_REGION`, `AWS_ACCOUNT_ID` and the exact
`BUILDER_VOLUME_ID` after verifying that the volume belongs to the selected
run-owned builder. This grants volume observation and modification of that one
volume; CAPA and worker roles do not need these permissions. After increasing
the EBS volume, extend the guest partition before preparing the cache.
AWS documents the volume resource scope in its
[EC2 authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ec2.html#amazonec2-ModifyVolume).


When replacing a worker with a different AMI or machine specification, use a
new `AWSMachineTemplate` name. CAPA rejects changes to an existing template's
specification. The harness accepts `--machine-template worker-image-v2` separately
from `--name worker`; it defaults to the claim name for compatibility. Delete
the old test claim and verify its cleanup before recreating that claim name with
the new template. Retain the old template while any Machine still references it.
This applies to Linux, Windows CPU, and Windows GPU workers.

Keep setup privileges on the administrative test runner. Restrict IAM resource
names to the run prefix and `AttachRolePolicy` to the intended managed policy;
network mutations belong to the isolated test VPC. Do not attach setup privileges
to CAPA or a worker. If setup fails, use the recorded state to clean up the
partially created environment.

Delete claims and wait for CAPA to delete their instances first. Then run:

```sh
python3 ~/Documents/cloud-provisioning/harness/e2e/aws/cleanup.py \
  --work-dir ~/Documents/cloud-provisioning/harness/e2e/.state/aws/example
python3 ~/Documents/cloud-provisioning/harness/e2e/aws/verify_cleanup.py \
  --work-dir ~/Documents/cloud-provisioning/harness/e2e/.state/aws/example
```

Cleanup checks the account and VPC ownership tag. It terminates remaining
instances only when both their VPC and run tag match, deletes recorded network
and IAM resources, and removes the saved STS session. Keep `resources.json` as
the resource inventory. CAPA bootstrap secrets are removed through the owning
provisioning workflow.
The cleanup helper also removes this run's recorded, tagged AMIs, snapshots and
asset bucket; it does not remove unrecorded images or arbitrary storage.
The verification helper reads AWS back and records absence of the VPC, recorded
images/snapshots, IAM roles/profile and bucket, plus termination of test instances.
Use the setup identity for both commands; the restricted sessions are removed
during cleanup.

## Running the AWS worker matrix

`cmd/awsrow` observes Linux workers using the selected distribution's runtime.
Its `-distro` and `-cni` flags accept k0s/kube-router (the default), k0s/Calico,
and MicroK8s/Calico. It checks the pinned distribution version, MicroK8s snap
revision where applicable, and the native runtime socket before importing test
images. The distribution builder supplies the import command, CRI endpoint and
site reuse checks; the AWS rig supplies guest execution through SSM. Reports
record the selected profile and CRI endpoint. Unsupported observer profiles are
rejected before contacting AWS.

Keep both the site and remote dialer images available in the harness host's
Docker store. They may differ. The runner stages both through the AWS rig and
requires a current, Ready Pod owned by the remote adoption DaemonSet before
running the workload matrix. `adoption.json` records the owning DaemonSet UID,
Pod UID and container image identities. It also requires the current Machine's
adoption Secret to acknowledge the SHA256 of its current peer payload, recording
only the Secret UID, resource version and desired/applied hashes. Missing or stale
acknowledgments fail the gate. The
[native receipt](validation/linux-adoption-receipts-results.json) and
[AWS row](validation/linux-adoption-row-results.json) reports describe observer
qualification on existing workers. Run fresh-boot and replacement tests
separately. Matrix results report the last convergence pass, not uninterrupted
traffic throughout the run.

The acknowledgment proves applied content at the observation time; it does not
identify which Pod wrote it or prove uninterrupted traffic during updates.
Exercise peer additions and removals separately. The bootstrap systemd service
remains available as a fallback, sharing the interface with the adopting dialer;
do not disable it to qualify adoption.

For a MicroK8s site created with `cmd/lab -rig vm -distro microk8s -cni calico
-product`, keep the setup helpers' API URL on port 16443 and import the site
with `cmd/importsite -api-port 16443`. Use the prepared Linux AMI with snapd
and Python 3 and the site's native MicroK8s join configuration. After CAPA
claims have been applied, select the matching observer:

```sh
go run ./cmd/awsrow -distro microk8s -cni calico \
  -work-dir .state/aws/example -site-work-dir .state/microk8s-site \
  -claims aws-microk8s-1,aws-microk8s-2 \
  -report-dir .state/aws/example/rows/microk8s-both
```

Pass `-api-port 16443` to `cmd/awsremove` for MicroK8s removal checks too.
That command defaults to 6443 for k0s; it must contact the same imported site.

The pinned MicroK8s profile uses snap revision `9063` (`v1.34.9`), bundled
Calico `v3.29.3`, and containerd `1.7.29`. Its CRI socket is
`/var/snap/microk8s/common/run/containerd.sock`; this snap does not expose a
`microk8s crictl` subcommand. The
[single-VM prerequisite evidence](validation/microk8s-prerequisite-results.json)
checks the installed version, socket, local kubelet API and native join-response
shape. It does not qualify remote CAPI joins or the AWS lifecycle matrix.

MicroK8s worker bootstrap retries the pinned snap installation up to five times,
with 15 seconds between failures. A failed cloud-init run still fails bootstrap;
inspect its status and `/var/log/cloud-init-output.log` through the provider's
out-of-band management. Bootstrap changes apply to new Machines: replace a
failed claim to test corrected userdata. The
[bootstrap validation report](validation/microk8s-snap-retry-results.json) separates
observed store failures, retry tests and subsequent native qualification.

The worker pattern waits for MicroK8s's native worker marker and the local
kubelet health endpoint before writing its bootstrap-success sentinel. It does
not use the control-plane `status --wait-ready` loop. The
[worker bootstrap report](validation/microk8s-worker-bootstrap-wait-results.json)
records the observed startup race and the scope of the replacement check.
Kubernetes Node readiness and network validation remain separate requirements.
The Linux VM and EC2 rows also require `cloud-init status --format json` to
report `done` with a clean exit and
`/run/cluster-api/bootstrap-success.complete` to exist. A Ready Node or working
container runtime alone cannot satisfy this bootstrap gate. A terminal
cloud-init error stops the row; an unavailable observer remains inconclusive.
For secure CAPA Linux userdata, the AWS row selects `CAPALinuxCommands`.
It additionally accepts cloud-init 26.1's exit code 2 only when the aggregate
and module reports contain no errors and the sole warning is the exact initial
`/etc/secret-userdata.txt` missing-include warning
[documented by CAPA](https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/v2.12.1/docs/book/src/topics/userdata-privacy.md#warning-messages).
Completed status and the success sentinel remain mandatory. Other recoverable
errors are not accepted, and generic Linux VM/image adapters retain the clean-exit
requirement. Cloud-init's
[exit-code documentation](https://docs.cloud-init.io/en/latest/explanation/return_codes.html)
explains why `done` alone does not establish success.
Windows requires a separate native completion observer and is not qualified
by this Linux check.

AWS network rows write each convergence attempt to `matrix-attempts.jsonl`,
including failed probes, elapsed time, cancellation and checks excluded by the
selected profile. `matrix.json` contains the final result. Preserve both files:
a passing final matrix does not establish uninterrupted survivor traffic.
Failure to write the attempt journal fails the row.

AWS rows also write `kubelet-access.json`, checking Kubernetes exec and logs
through each control plane against every measured worker and site probe Pod.
These checks are separate from the workload probes run through out-of-band
management. A passing pod-network matrix cannot establish kubelet reachability;
a failed management check fails the row.

The VM `cmd/lab -check` harness applies the same gate after placement checks,
after removal and recreation, and at the final matrix. Its `events.jsonl`
records `kubelet-access` events with the stage and every control plane, Node,
operation and result. Lifecycle tests also require a passing baseline before
removal. A missing path set or failed exec/log request fails qualification.
Checks during an intentional control-plane outage remain separate; the final
management gate runs after the control planes have returned.

MicroK8s workers advertise their allocated WireGuard address to the kubelet.
The control planes need a route to that address on TCP 10250. EC2's private NIC
address can be unreachable from the site and can overlap another cloud's
network. The AWS provider ID still identifies the instance independently of
the advertised kubelet address. Updating the join pattern takes effect on
new Machines; existing workers require replacement to receive it.
The [kubelet address report](validation/microk8s-kubelet-address-results.json)
and [continuous replacement report](validation/microk8s-tunnel-lifecycle-results.json)
record the qualified versions, endpoint placement, native checks, and coverage.
The [three-worker replacement report](validation/microk8s-remote2-replacement-results.json)
records qualification after replacing the remaining private-address worker,
including survivor streams and direct kubelet access through all control planes.
The [placement report](validation/microk8s-tunnel-placement-results.json) separates
converged network and kubelet access from continuous packet delivery. Endpoint
handover is not qualified for uninterrupted traffic while that survivor gate fails. The
[tunnel handover design](tunnel-handover.md) describes the receive-ownership
constraint and the staged protocol required before lifting this limitation.

During endpoint retirement, physical-source traffic must use the relay after
the remote acknowledges moving that source to the relay peer. Calico VXLAN can
use a physical source even when its destination is a WireGuard address. The
dialer therefore reserves a companion routing table (`route-table + 65536`,
66053 by default), with an exact local tunnel-source rule immediately before
the ordinary table rule. Its direct host routes preserve explicitly tunnel-bound
traffic; physical-source traffic follows the ordinary relay routes. The companion
routes and rule are removed on promotion or endpoint teardown. Both tables must
be reserved for the dialer. See the [source-routing qualification](validation/microk8s-source-routing-results.json)
for native evidence and remaining continuity gates.

Native `microk8s join --worker` can return before the worker's Kubernetes Node
exists. Observe Node registration before waiting for Ready; a transient NotFound
does not justify replaying the join. Inspect bundled Calico's addresses on the
Kubernetes Node annotations rather than assuming a Calico Node CRD exists.
The [two-VM native check](validation/microk8s-native-join-results.json) verifies
worker joining, both active local kubelet API contexts, and basic Node removal
after stopping the worker. The [single-NIC VM lifecycle campaign](validation/microk8s-lifecycle-results.json)
also verifies rendered CAPI userdata, tunnel traffic, both remote replacements,
survivor matrices and four endpoint placements. That campaign uses the VM
infrastructure provider on an AWS runner; it does not qualify CAPA workers.

For an unjoined Ubuntu AWS image, compose the shared
[`microk8s-prerequisites.sh`](../images/linux/microk8s-prerequisites.sh) and
[`verify-microk8s-prerequisites.sh`](../images/linux/verify-microk8s-prerequisites.sh)
with `aws/bake.py`'s provider bootstrap preparation. The shared layer installs
snapd and Python 3 while rejecting MicroK8s cluster state at capture. AWS CLI,
cloud-init handling, IAM access and AMI capture remain in the AWS adapter.
Installing the MicroK8s snap and joining the selected site still occur at first
boot; these prerequisite images do not yet supply a cached snap/runtime layer.

These selectable profiles are not a claim that every AWS matrix has passed.
[Runtime observer validation](validation/awsrow-distribution-results.json)
includes live k0s checks through QEMU guest-agent and AWS SSM transports, plus
unit checks for distribution isolation. A fresh MicroK8s AWS join, network,
removal and replacement campaign remains required. Windows uses the separate
[Windows join and network observers](windows.md), not this Linux row runner.

Use an existing k0s/Calico VM site prepared with `cmd/lab -rig vm -distro k0s
-cni calico -product`. Configure its chart with
`providerManagerNamespace=capa-system` and the private binary URL and SHA-256
from the asset helper. Run the following from `harness/e2e` after preparing and
promoting the AMI. First run `aws/session.py --work-dir <absolute-run-directory>`
from the sourced setup shell to create the derived harness session as well as
renewing CAPA's session:

```sh
python3 aws/install.py --work-dir .state/aws/example --api-server https://10.10.0.10:6443
python3 aws/identity.py --work-dir .state/aws/example --api-server https://10.10.0.10:6443
go run ./cmd/importsite -name cldt-example -work-dir .state/site
python3 aws/claim.py --work-dir .state/aws/example --name aws-k0s-1 --api-server https://10.10.0.10:6443
python3 aws/claim.py --work-dir .state/aws/example --name aws-k0s-2 --api-server https://10.10.0.10:6443
go run ./cmd/awsrow -cni calico -work-dir .state/aws/example -site-work-dir .state/site \
  -claims aws-k0s-1,aws-k0s-2 -report-dir .state/aws/example/rows/both
```

The import observes the live site and publishes the CAPI connection contract.
CAPA launches workers; the harness observes their provider IDs and Node UIDs.
Each row verifies the selected native release, product label and taint, approved AMI,
one EC2 ENI and one guest physical NIC before measuring traffic. Guest execution
uses SSM; the site guests use serial QGA. The harness imports the test dialer
image through private S3 without changing bootstrap configuration.

To test removal, replacement and survivor connectivity:

```sh
go run ./cmd/awsremove -work-dir .state/aws/example -site-work-dir .state/site \
  -claim aws-k0s-1 -report .state/aws/example/remove-1.json
go run ./cmd/awsrow -cni calico -work-dir .state/aws/example -site-work-dir .state/site \
  -claims aws-k0s-2 -report-dir .state/aws/example/rows/survivor-1
python3 aws/claim.py --work-dir .state/aws/example --name aws-k0s-1 --api-server https://10.10.0.10:6443
go run ./cmd/awsrow -cni calico -work-dir .state/aws/example -site-work-dir .state/site \
  -claims aws-k0s-1,aws-k0s-2 -report-dir .state/aws/example/rows/readded-1
```

Removal follows the Machine's infrastructure reference and waits for the claim,
Machine, AWSMachine, Node, bootstrap/adoption Secrets and peer fields to disappear.
It independently confirms EC2 termination. The survivor row must pass 102 checks;
the restored row must pass 140. Compare `instanceID` and `nodeUID` in the
before/after `bindings.json`: both must change for the replaced claim.
Repeat for the other claim. Report paths must be new to preserve prior evidence.

Then run each endpoint placement with both workers attached:

```sh
for placement in control-plane one-worker two-workers all-nodes; do
  go run ./cmd/awsrow -cni calico -work-dir .state/aws/example -site-work-dir .state/site \
    -claims aws-k0s-1,aws-k0s-2 -placement "$placement" \
    -report-dir ".state/aws/example/rows/placement-$placement" || break
done
python3 aws/results.py --work-dir .state/aws/example --baseline both \
  --output .state/aws/example/results.json
```

Placement changes use Helm with existing values preserved. The gate checks
published endpoint membership and actual WireGuard devices on every site node
before measuring traffic. The default three-minute endpoint-retention window
remains enabled; departing endpoints also wait for remote acknowledgements.
The result exporter validates both replacements,
unchanged survivor identities, matrix counts and placement records; it exports
only selected metadata and matrix hashes, never raw observation snapshots.


`aws/session.py` renews one-hour derived CAPA and harness sessions using the setup
identity. Run it from the sourced setup shell, then rerun `aws/identity.py` to
publish the renewed CAPA session. Session files are replaced atomically so active
SSM checks can reload them. Keep all run evidence private: observation snapshots
can include expiring download URLs in workload arguments.

For runtime coverage, build commands with `go build -cover`, run them with
`GOCOVERDIR` pointing at an existing private directory, then use
`go tool covdata percent -i <directory>`. Use `make coverage` separately for
controller and harness unit coverage. To include measurements in the exported
result, pass `aws/results.py` the `--unit-coverage-log` from `make coverage`,
the `--runtime-coverage` output from `go tool covdata percent`, and optionally
`--dialer-binary` to record the tested executable's SHA-256.

If specifying `-coverpkg`, include the command package as well as its libraries
so the executable emits runtime coverage.
Treat an empty coverage directory as unavailable evidence, not zero failures.

## Private bootstrap binaries

EC2 cannot reach the VM lab's local download server. `harness/e2e/aws/assets.py`
creates a private, public-access-blocked S3 bucket, uploads test binaries with
server-side encryption, and saves four-hour presigned URLs in `asset-urls.json`.
It signs with an STS federation session restricted to `GetObject` on the bucket's
`downloads/` prefix. Pass a URL and the binary's SHA-256 to the chart's
`dialerBinary` values. URLs are bearer credentials; keep them out of logs.

Renew these URLs separately from CAPA and SSM sessions. Before a later claim or
replacement, rerun `assets.py` with the intended binaries and update the chart's
`dialerBinary.<architecture>.url` together with the matching digest. Writing a
new `asset-urls.json` does not update the running controller. Temporary signing
credentials can expire before the URL's advertised deadline; see
[S3 presigned URL expiration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html#PresignedUrl-Expiration).

For a bounded integration run that needs the **same existing Linux binary**,
`harness/e2e/aws/renew_artifact.py` downloads the currently configured S3 object,
verifies its configured SHA-256, signs it with a new exact-object session, and
conditionally updates only that URL argument on the running controller. Run it
with setup credentials loaded through `source-me.sh` and a new evidence directory:

```sh
python3 harness/e2e/aws/renew_artifact.py \
  --work-dir harness/e2e/.state/aws/windows-v2 \
  --api-server https://10.10.0.10:6443 \
  --evidence-dir harness/e2e/.state/artifact-renewal-NEW
```

The conditional update rejects a concurrent Deployment change. It starts a
controller rollout and retains a private patch containing the new URL; do not
publish that file. The helper does not upload a replacement binary or recreate
a claim. Keep Helm's configured URL synchronized before a later upgrade, since
this integration helper updates the live Deployment directly. It requires the
setup identity's existing-object `s3:GetObject` and `sts:GetFederationToken`
permissions, not additional worker permissions. A claim still waiting for its
first bootstrap Secret can resume after renewal without being recreated.

For new cloud-config bootstrap documents, the controller rejects a dialer URL
whose SigV4 timestamp and lifetime already indicate expiration. It withholds
the bootstrap Secret and reports an error without logging the credential-bearing
URL. This does not validate S3 permissions, revocation, or a signing session's
earlier expiry, nor guarantee that a URL survives a delayed instance launch.
Existing bootstrap Secrets are not rewritten: a failed first boot needs a new
Machine after the artifact configuration is corrected. The test helper's
four-hour URLs are intended for bounded runs, not unattended long-term
provisioning.

Qualify bootstrap publication, dialer adoption, network reachability and survivor
traffic separately. A same-name claim recreation does not establish automatic
Machine repair while retaining the original claim UID. The harness imports its
deployed dialer images before adoption; this does not qualify preloaded-image
startup or unattended artifact renewal. Sampled traffic establishes delivery
only for the recorded requests and observation window.

See the [Linux lifecycle](validation/linux-capi-egress-results.json),
[survivor traffic](validation/linux-capi-survivor-results.json), and
[same-name replacement](validation/linux-capi-replacement-results.json) reports
for tested versions, interventions, identity checks and qualification limits.

The [asset policy](iam/assets.json) is for the setup/publishing identity.
`GetFederationToken` requires IAM-user credentials; a runner already using an
assumed role must instead use its role session with a suitably restricted S3
policy. The EC2 rig stages stdin and image imports under `transfers/` using the
harness policy, a 15-minute URL, and object deletion after command completion.
CAPA and worker roles need no S3 permission just to fetch a presigned
binary URL. This binary transport is separate from CAPA's private bootstrap
Secrets Manager transport and from an OKD Ignition storage implementation.

The cleanup command removes the tagged asset bucket and its objects. The helper
is intended for the bounded test upload set, not arbitrary versioned buckets.

## Management and failure testing

CAPI does not provide a universal out-of-band guest console. EC2's control API
can operate on an instance independently of its guest network. SSM runs through
the instance's existing ENI and outbound HTTPS; it becomes unavailable if that
network path is severed. Local QEMU VMs use serial QGA and can retain management
access with their sole Ethernet NIC down.

`cmd/awsnode` selects Linux or Windows command execution from EC2's platform
metadata. The harness policy permits the AWS-owned `AWS-RunShellScript` and
`AWS-RunPowerShellScript` documents while retaining run-tag restrictions on
target instances. This enables diagnostics; it does not establish Windows
bootstrap or joining support. Renew existing derived sessions after changing
the policy. The session helper omits descriptive statement IDs from its wire
policy to fit STS's compressed-policy limit without changing permissions.

For long image preparation, set `awsnode -timeout 50m` before the command
arguments. The shared AWS rig passes the remaining caller deadline to the SSM
document's `executionTimeout` for both guest types (up to AWS's 48-hour limit).
Callers without a deadline use ten minutes; `awsnode` defaults to five minutes.
This execution limit is distinct from SSM command delivery and local polling.
A local timeout does not cancel the remote command. Observation errors include
its command ID: inspect it with `aws ssm get-command-invocation --command-id ID
--instance-id INSTANCE`, and check any durable guest receipt before retrying.
Redirect verbose image-pull output to a
guest file, because SSM's inline output is bounded.
Output-limit errors include the original SSM command ID and the stdout/stderr
byte counts. Inspect that invocation and retrieve its retained output through
the configured evidence path; do not rerun a mutating command merely to obtain
its output. The error message does not include command output or credentials.
The [timeout validation](validation/aws-ssm-timeout-results.json) separates
regression coverage from native command submission and completion evidence.

The shared matrix must distinguish an EC2 power operation, a security-group
WireGuard partition, and a physical NIC cut. An unsupported failure mechanism
must be reported explicitly, not silently replaced with a different one.

References: [CAPA existing infrastructure](https://cluster-api-aws.sigs.k8s.io/topics/bring-your-own-aws-infrastructure.html),
[CAPA v2.12.1 machine schema](https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/v2.12.1/api/v1beta2/awsmachine_types.go),
[CAPA identity schema](https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/v2.12.1/api/v1beta2/awsidentity_types.go),
[secure bootstrap image requirements](https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/v2.12.1/docs/book/src/topics/userdata-privacy.md).

## Recreating a gateway attachment

For an isolated harness replacement that needs the same gateway topology as an
existing worker, use `harness/e2e/aws/gateway_request.py`. It reads an existing
gateway request as the topology template, observes both live Machine/Node/EC2
bindings, checks run ownership and one primary NIC, and creates a new request
with the replacement's identities. Linux and Windows use the same AWS binding
model; the template supplies the distribution's existing transport settings.

```sh
python3 harness/e2e/aws/gateway_request.py \
  --work-dir PRIVATE_RUN_DIRECTORY --api-server https://ISOLATED_API:6443 \
  --worker WORKER_MACHINE --worker-uid CURRENT_MACHINE_UID \
  --gateway GATEWAY_MACHINE --gateway-uid CURRENT_GATEWAY_MACHINE_UID \
  --template-configmap EXISTING_GATEWAY_REQUEST \
  --intent NEW_PRIVATE_INTENT.json
```

The helper requires an observed Node association and a matching live gateway;
it does not provision a gateway or invent a topology. It preserves the template's
CNI and TCP-port settings. The intent file is created exclusively before the
ConfigMap submission. If submission fails, inspect the saved intent and cluster
state before retrying. Wait for the attachment to become Ready; request creation
alone does not prove forwarding. Preserve each completed or failed operation's
intent file and use a new path for each replacement.

For Windows gateway-mode trials, qualify the attachment before creating workload
probes. Node readiness and a delivered host WireGuard peer document alone do not
prove that the gateway forwards the worker's pod network:

```sh
python3 harness/e2e/aws/windows_join.py \
  --work-dir PRIVATE_RUN_DIRECTORY --api-server https://ISOLATED_API:6443 \
  --awsnode PATH_TO_AWSNODE --name WORKER_MACHINE --windows-version 2025 \
  --require-peer-delivery --require-gateway --output NEW_JOIN_EVIDENCE.json
```

Use `2022` for Windows Server 2022. `--require-gateway` requires at least one
active attachment for the current Machine UID; all its active attachments must
be Ready, bound to the current Node UID and free of a deletion timestamp.
Completed attachments do not satisfy the check. Follow this checkpoint with the
ordinary-pod traffic matrix. Failed native guest observations retain executor
stderr in a separate private `.guest.stderr` artifact beside the report; inspect
that evidence before repeating an observation.

Qualify Windows CPU lifecycle, GPU startup caching and uninterrupted survivor
traffic independently. Bake the runtime pause image alongside the application
and device-plugin images; application cache hits alone do not establish a
complete startup cache. GPU qualification also requires the first hardware
render/NVENC workload on a fresh VM. A successful workload or removal does not
waive a failed traffic or observation-interval gate.

| Qualification | Evidence |
| --- | --- |
| Windows Server 2022 CPU lifecycle | [Join, attachment, traffic and removal](validation/windows-capi-survivor-2022-results.json) |
| Windows Server 2025 CPU lifecycle | [Join, attachment, traffic and removal](validation/windows-capi-survivor-2025-results.json) |
| Windows Server 2025 GPU cache lifecycle | [GPU execution and remaining lifecycle failures](validation/windows-capi-gpu-cache-2025-results.json) |
| Complete Windows Server 2025 startup cache | [Runtime pause and application cache validation](validation/windows-capi-gpu-startup-cache-2025-results.json) |

Gateway withdrawal must preserve the retiring worker's API connectivity until
it acknowledges direct tunnel routes. The controller stages this change with
`retiringWorker` on the existing projection, preserving other recipients' gateway
routes. Only the exact worker's acknowledgement permits global withdrawal;
all recipients must then acknowledge before forwarding resources and CAPI hooks
are released. Controllers that ignore `retiringWorker` cannot perform this
staged transition.
The [Windows 2025](validation/windows-capi-gpu-staged-withdrawal-2025-results.json)
and [Windows 2022](validation/windows-capi-staged-withdrawal-2022-results.json)
validation reports distinguish acknowledgment ordering and completed removal
from survivor packet delivery. Both retain UDP failures and therefore do not
qualify uninterrupted lifecycle traffic. CPU results do not qualify GPU caching,
fragmented UDP or repeated reliability. Use the
[withdrawal recorder](../harness/e2e/observe/README.md#verifying-staged-gateway-withdrawal)
with original object UIDs and preserve incomplete observations.

### Running the Windows lifecycle sequence

`harness/e2e/aws/windows_lifecycle.py` runs the sequence for either
Windows version. Start the [out-of-band survivor observer](../harness/e2e/observe/README.md)
first, using existing ordinary probe Pods and trusted VM/SSM executors. The runner
requires successful recent samples from every directed pair, matched to the
configured Pod names and Node UIDs. It rejects a completed or stopped observer.

Create a private JSON configuration with these fields, replacing the placeholders:

```json
{
  "apiServer": "https://ISOLATED_API:6443",
  "bastion": "TEST_BASTION",
  "name": "UNIQUE_WORKER_CLAIM",
  "windowsVersion": "2022",
  "workDirectory": "/absolute/private/aws-run",
  "siteDirectory": "/absolute/private/vm-site",
  "awsnode": "/absolute/path/to/awsnode",
  "awsremove": "/absolute/path/to/awsremove",
  "gateway": "EXISTING_GATEWAY_MACHINE",
  "gatewayUID": "CURRENT_GATEWAY_MACHINE_UID",
  "gatewayTemplate": "EXISTING_GATEWAY_REQUEST_CONFIGMAP",
  "probeNamespace": "EXISTING_PROBE_NAMESPACE",
  "sourcePod": "EXISTING_PROBE_FOR_THIS_WINDOWS_VERSION",
  "observerDirectory": "/absolute/private/running-survivor-observer",
  "survivorTargets": ["LINUX_POD=NODE_UID", "WINDOWS_POD=NODE_UID"]
}
```

The source Pod must use one digest-pinned probe container and have a same-name
Service. Its Node build must match the requested Windows version. The runner
copies the container recipe and tolerations into a new Pod and omits the source
Pod's service-account mounts. Use `2025` for Windows Server 2025. Renew the run's
derived credentials and pull access before starting a long trial.

From `harness/e2e`, run:

```sh
python3 aws/windows_lifecycle.py --config PRIVATE_CONFIG.json --output NEW_PRIVATE_TRIAL_DIRECTORY
```

The sequence creates one claim, waits for its Node, creates the UID-bound gateway
request, qualifies Ready attachments and native join, creates the workload once,
runs the matrix, then removes the claim through CAPI. It verifies survivor and
gateway-lease convergence and records `operation-window.json` before stopping
the observer. The final `operation.json` separates `lifecyclePassed` from
`survivorPassed`; combined `passed` requires both. Native collection failures
leave a failed final report. With the legacy external observer, the survivor
status remains `pending` and combined `passed` remains false; the runner does not
rewrite that report when an external assessment finishes. Assess the completed
observer against that operation window, then
remove the owned probe Service, machine template and temporary observer tools.
Preserve the attachment retirement records.

A failed stage preserves the original resources and evidence for diagnosis.
Inspect the recorded process and original CAPI/SSM operation before any follow-up;
rerunning with another output directory is not a resume mechanism. The output
directory must be new. Use the release-specific validation reports above to
check which lifecycle and survivor gates are qualified.

## Private Windows publisher image

For isolated Windows testing, `harness/e2e/aws/publisher.py` accepts an extracted
OCI image layout (`--layout`) or Docker image archive (`--archive`), the run
inventory and an explicit cluster API server. It
creates or verifies the run-owned private ECR repository, pushes an immutable
digest-derived tag, checks the registry digest, and publishes an expiring
`imagePullSecret` in `cloud-provisioning`. Configure the recorded `publisherImage`
and `publisherPullSecret` as `windowsPublisher.image` and the dialer pull secret.
Run the helper again to renew the pull credential before its recorded session
expiration. This is a test credential workflow, not an automatic production
credential rotation mechanism.

Use `--component calico-node-windows` to publish a
[derived Windows Calico image](../images/calico/README.md). This selects the
separate `${RUN_ID}/calico-node-windows` repository and a pull Secret in
`kube-system`. The helper records `calicoWindowsImage`, `calicoWindowsRepository`,
`calicoWindowsPullSecret` and `calicoWindowsPullSessionExpiresAt` without replacing
the tunnel publisher's inventory. Configure the image and pull Secret through
k0s's Calico settings so controller reconciliation preserves them. The default
component remains `windows-publisher`.

Use `--component windows-gpu-device-plugin` for the
[candidate WDDM plugin image](../providers/windows-gpu-device-plugin/README.md).
This selects `${RUN_ID}/windows-gpu-device-plugin`, stores
`windowsGPUPluginImage` and `windowsGPUPluginPullSecret`, and publishes the
pull Secret in `cldt-windows-gpu`. Each component's pull session can read only
its own repository. Cleanup and cleanup verification include all recorded
component repositories.

Use `--component windows-gpu-probe --layout PATH` to publish a digest-pinned
ordinary Windows GPU workload candidate. This selects `${RUN_ID}/windows-gpu-probe`,
records `windowsGPUProbeImage` and `windowsGPUProbePullSecret`, and creates the
pull Secret in `cldt-windows-gpu`. Set both the test Job image and the GPU
observer’s `--image` to the recorded digest. Publishing does not qualify an image;
qualification requires the first workload attempt on a fresh VM.

For separate Server 2022 and 2025 workload bases, add `--windows-version 2022`
or `--windows-version 2025`. The helper verifies the OCI manifest/config digests
and requires the selected base build (20348 or 26100) before contacting AWS.
It records `image`, `platform` and pull information under
`windowsGPUProbeByWindowsVersion.YEAR`, preserving the legacy image field and
the other release's record. Both releases share the owned component repository
and pull Secret. Use the selected record's digest in its cache recipe, Job and
observer; a copied Server 2025 image cannot qualify a Server 2022 worker.
Pass the same version when using `--renew-pull-only` for that record. This
base-build check does not replace native container compatibility or GPU tests.
The [publication validation report](validation/windows-workload-platform-publication-results.json)
documents cross-release rejection and publication-validation limits. Use
[`select_workload(inventory, recipe, year)`](../harness/e2e/aws/windows_gpu_workload.py)
to require that the release-specific publication is present in the matching
cache recipe. It returns the recorded image and pull Secret and rejects a
missing versioned record instead of falling back to the legacy image. Pass
that image to both the Job and GPU observer; the native first-application gate
still determines whether the workload actually runs.
The [selection tests](validation/windows-gpu-workload-selection-results.json)
use the observed publication and native cache receipt as fixtures.
Also call `require_plugin_cache(daemonsets, recipe, year)` with the site's live
device-plugin DaemonSets before provisioning a cache trial. The Windows 2022
and 2025 profiles use different images. This check requires the selected
profile's pinned images in the recipe and rejects changed build selectors;
it does not replace fresh-VM cache-hit and GPU execution checks.
For a profile using `index.docker.io`, preload both its declared reference and
the equivalent `docker.io` reference. Containerd 2.3.2 normalizes the hostname
for CRI lookup, while the native `ctr` cache can retain only the declared name.
The preflight checks both references; they point to the same pinned content.

Render [publisher.json](iam/publisher.json) with `AWS_REGION`, `AWS_ACCOUNT_ID`
and `RUN_ID` using the same renderer as the other policies. Attach it to the
setup publisher identity. The current helper uses `sts:GetFederationToken`, so
invoke it with the setup IAM-user credentials used by the harness, not an
already-assumed-role session. A production role-based publisher should use a
separate read-only pull role and `AssumeRole` instead.

For a later VM trial, refresh access to an already recorded immutable image
without providing its archive or layout:

```sh
python3 harness/e2e/aws/publisher.py --work-dir harness/e2e/.state/aws/windows-v2 \
  --component windows-publisher --renew-pull-only \
  --api-server https://10.10.0.10:6443
python3 harness/e2e/aws/publisher.py --work-dir harness/e2e/.state/aws/windows-v2 \
  --component calico-node-windows --renew-pull-only \
  --api-server https://10.10.0.10:6443
```

Use setup credentials loaded through `source-me.sh`. Renewal requires the
recorded digest and an existing repository with this run's ownership tag. It
verifies that digest using a fresh repository-scoped read credential before
updating the isolated cluster's pull Secret. It does not create a repository,
push an image, or change the recorded image digest. Renew each required
component before starting the observation window; the publisher, Calico and
GPU components have separate repositories and pull Secrets.

An [ECR token inherits its principal’s permissions](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html).
The setup push token therefore stays in a temporary local Docker configuration.
The cluster receives a separate session token restricted to ECR authentication
and the three image-read operations on the selected repository. It receives no IAM
access keys. The policy follows [AWS’s image-push permissions](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-push-iam.html).
Cleanup records the repository before pushing, verifies its ARN and ownership
tag before deletion, and checks repository absence afterward.

The Windows subnet gateway needs both native CNI UDP ingress and TCP ingress for
the configured Kubernetes API port from each worker's native `/32`. Include the
distribution's other control-plane listeners in the request's `tcpPorts` field:
k0s's default Konnectivity listener requires `[8132]`. Without it, a worker can
be Node Ready while its Konnectivity agent and API-server-to-kubelet operations
fail. The gateway controller manages these as distinct leases using the
[gateway ingress policy](iam/gateway-ingress.json). The
[Konnectivity validation](validation/windows-konnectivity-ingress-results.json)
covers agent readiness and kubelet access. IAM scopes that policy to the
owned security group; protocol, port and source restrictions are enforced by the
adapter's exact rule construction and ownership journal. See the
[gateway runtime](windows-gateway.md#enable-the-aws-attachment-controller) for its
current validation limits.

## EC2 runner capacity

An EC2 host can provide capacity for the unchanged seven-VM harness while local
fleets remain running. Select an x86_64 instance with nested virtualization
support and sufficient memory for guests, appliances and builds. Enable
`CpuOptions.NestedVirtualization=enabled` at launch, then verify `/dev/kvm` and
hardware acceleration before starting the lab. AWS documents supported types
and limitations in its [nested virtualization guide](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html).
Follow the [isolated runner instructions](../harness/e2e/runner/README.md);
each Kubernetes guest still has one data NIC and serial management.

Use a separate test role with an explicit base-AMI allowlist, approved subnet
and security group, exact instance-profile pass-role permission, and a unique
runner ownership tag. Render the [runner policy](iam/runner.json) for that role,
using the standard account, region, AMI, subnet, security-group and instance-profile
values, plus `RUNNER_INSTANCE_TYPE`, `RUN_ID` and `ASSET_BUCKET`.
Its artifact access is limited to `runners/${RUN_ID}/*` in that bucket, including
permission to abort an incomplete multipart upload. Use the source-principal
trust template to obtain short-lived operator sessions. Do not add an
unprepared host AMI to the worker role merely to launch a runner. A successful
EC2 dry run establishes authorization, not capacity availability or KVM support
inside the guest. Retain instance identity immediately after launch, bound the
campaign duration, and remove its owned instance and volumes after collecting
evidence.

Compare current compute prices together with EBS, public IPv4 and transfer
costs. Verify cost-allocation tag activation before interpreting tagged Cost
Explorer totals: an inactive tag can yield a misleading zero. Account-wide
reported usage can provide context but excludes unreported usage and is not
project attribution. The [capacity audit](validation/aws-runner-capacity-results.json)
records candidate prices, billing limitations and the rejected worker-role
base-AMI dry run. It does not establish a completed EC2 harness campaign.

The [dedicated runner validation](validation/aws-runner-authorization-results.json)
checks the approved launch and rejection of a different instance type or
ownership tag using a separate role. It also records host KVM availability,
the shutdown timer and verified artifact transfers. Host readiness is separate
from successful nested VM bring-up and lifecycle-matrix results.
