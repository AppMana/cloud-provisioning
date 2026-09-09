# CAPA Windows secure bootstrap extension

This bounded extension targets CAPA **v2.12.1**. It adds a `powershell` bootstrap
format that keeps CAPA's Secrets Manager chunk creation, ownership fields and
cleanup, while replacing the Linux cloud-init consumer with EC2Launch PowerShell.
It is not part of unmodified upstream CAPA. Fresh Windows CAPI joins pass the
[image and association checks](../../docs/validation/windows-fresh-image-join-results.json);
worker removal and replacement have separate
[scoped VM evidence](../../docs/windows.md#validation-boundaries). Workload
networking and gateway failover remain unqualified.

Apply to a clean checkout, then build/test in that checkout:

```sh
git clone --branch v2.12.1 --depth 1 https://github.com/kubernetes-sigs/cluster-api-provider-aws.git /tmp/capa-windows
./providers/capa/apply.sh /tmp/capa-windows
cd /tmp/capa-windows
go test ./pkg/cloud/services/userdata
go test ./pkg/cloud/scope -run 'TestPowerShellUsesSecrets|TestUseSecretsManager|TestCompressUserData'
```

The overlay and patch are maintained together. `apply.sh` checks the exact tag
and refuses a dirty checkout. Deploying the resulting controller requires the
isolated harness's normal image and integration validation; these instructions
do not update a production controller.

The patch and overlay apply cleanly to the target release. The renderer and
format tests pass, and the controller compiles with the extension. VM consumer
checks use real encrypted, multi-chunk Secrets Manager payloads on Server 2022
and 2025. The patched controller has also passed an
[isolated VM-cluster rollout](../../docs/validation/capa-windows-rollout-results.json),
with the running image ID matching the local build. Use the
[Windows validation boundaries](../../docs/windows.md#validation-boundaries)
for the subsequent first-boot, join, removal and replacement evidence; the
controller rollout by itself does not establish those behaviors.

## Bootstrap contract

The CAPI bootstrap Secret contains raw UTF-8 PowerShell in `value`, with
`format: powershell`. CAPA compresses that payload into its encrypted secret
chunks. EC2 userdata contains only the chunk prefix, count, region and consumer.
The guest downloads all chunks, decompresses the script into a protected file,
deletes the encrypted secrets, executes the script and removes the temporary
file. UTF-8 is written with a BOM for Windows PowerShell 5.

The wrapper requires **EC2Launch v2** and uses `<detach>true</detach>` with
`<persist>false</persist>`. EC2Launch normally starts SSM after inline userdata;
detaching lets management start while a join is still running. A successful
launch-agent exit therefore does not establish bootstrap success: the harness
must check the guest installation, Node readiness and CAPI association.
The secure consumer writes an atomic, non-secret receipt at
`C:\ProgramData\CloudProvisioning\Bootstrap\status.json`. It records the EC2
instance ID, SHA-256 of the bootstrap chunk prefix, stage and terminal state.
Success is written after the child script exits successfully and its temporary
credential file is removed. Failures record the stage without serializing an
exception. The observer also checks the protected directory, instance identity
and absence of the temporary script; it does not treat an older instance's
receipt as evidence for a new VM.

`cmd/awsnode -bootstrap-status` observes this contract through the selected
machine adapter. Add `-bootstrap-wait 15m -timeout 16m` to wait for completion.
New Windows lifecycle rows require this receipt through
`aws/windows_join.py --require-bootstrap-completion`. They need a CAPA controller
built from the updated consumer; older controllers do not write the receipt.
The existing Windows image prerequisites remain unchanged.
[Native receipt tests](../../docs/validation/windows-bootstrap-contract-results.json)
cover synthetic success and script failure on both Windows releases. Fresh CAPI
launches with this consumer also passed receipt, Node-association, peer-delivery
and CAPI removal checks on Server 2022 and 2025. Those joins required renewal of
expired private image pull credentials. Workload traffic, continuous survivor
availability and Windows replacement remain separate qualification gates.

See [AWS's execution-order and detach documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2launch-v2-settings.html).
[Native Windows 2022 and 2025 first boots](../../docs/validation/capa-windows-detached-results.json)
confirmed SSM access before worker installation on both versions. These
execution-order observations do not replace the subsequent join checks.

The extension requires the Secrets Manager backend and refuses the insecure
skip option or the Parameter Store backend. It disables EC2 userdata compression
for this format. The guest requires Windows PowerShell, AWS.Tools.SecretsManager 5.0.268 and the
protected image directory established by the [Windows image recipe](../../images/README.md).
The [AWS image layer](../../images/aws/windows/prepare-bootstrap.ps1) verifies or
installs the pinned AWS Tools modules during image preparation. Amazon's base
images differ: Server 2022 carried the monolithic AWSPowerShell module, while
Server 2025 carried modular AWS Tools. The consumer uses the modular dependency
on both builds.
It explicitly uses instance-profile credentials. No package installation occurs
in the consumer.

The instance role needs the existing [worker bootstrap policy](../../docs/iam/worker-bootstrap.json):
`secretsmanager:GetSecretValue` and `secretsmanager:DeleteSecret` for the run-owned
CAPA secret prefix. A customer-managed KMS key additionally needs its matching
decrypt authorization. The standard harness uses the AWS-managed Secrets Manager
key. The extension does not broaden the worker's secret access.

CAPA still deletes remaining secret entries when a Machine joins or is deleted.
A failed script is a failed bootstrap; replace the disposable Machine to mint
fresh identity instead of replaying a partially consumed credential document.
