# Verify the AWS CNI image layer without starting services or changing networking.
param([Parameter(Mandatory=$true)][string]$Recipe)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$root='C:\ProgramData\CloudProvisioning'
$build=[Environment]::OSVersion.Version.Build
if ($build -notin @(20348,26100)) { throw 'Unsupported Windows build' }
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Image directory must have a protected ACL' }
if ((Get-Item -LiteralPath $root -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Image directory is a reparse point' }
$expected=Get-Content -LiteralPath $Recipe -Raw | ConvertFrom-Json
$record=Get-Content -LiteralPath (Join-Path $root 'aws-vpc-cni-image.json') -Raw | ConvertFrom-Json
if ($record.schemaVersion -ne 1 -or $record.windowsBuild -ne $build) { throw 'AWS CNI preparation record does not match this image' }
foreach ($field in @('archiveSHA256','pluginSHA256','connectorSHA256')) {
    if ($expected.$field -cnotmatch '^[a-f0-9]{64}$' -or $record.$field -cne $expected.$field) { throw "AWS CNI preparation checksum differs: $field" }
}
foreach ($target in @(
    @{path='C:\opt\cni\bin\vpc-bridge.exe';sha=$expected.pluginSHA256},
    @{path='C:\Program Files\Amazon\EKS\bin\aws-vpc-cni-k8s-connector.exe';sha=$expected.connectorSHA256}
)) {
    $path=$target.path
    while ($path) {
        if ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'AWS CNI destination contains a reparse point' }
        $parent=Split-Path -Parent $path
        if ($parent -eq $path) { break }
        $path=$parent
    }
    if ((Get-FileHash -LiteralPath $target.path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $target.sha) { throw 'Installed AWS CNI checksum mismatch' }
}
$kubeconfig='C:\var\lib\k0s\kubelet.conf'
if ($record.kubeconfigPath -cne $kubeconfig -or [Environment]::GetEnvironmentVariable('KUBECONFIG','Machine') -cne $kubeconfig) { throw 'AWS CNI connector kubeconfig path mismatch' }
$record | ConvertTo-Json -Compress
