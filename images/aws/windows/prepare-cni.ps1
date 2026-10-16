# Stage AWS's native CNI assets in a fresh, unjoined k0s image.
param(
    [Parameter(Mandatory=$true)][string]$Archive,
    [Parameter(Mandatory=$true)][string]$Recipe
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$root='C:\ProgramData\CloudProvisioning'
$inputRecipe=Get-Content -LiteralPath $Recipe -Raw | ConvertFrom-Json
foreach ($field in @('archiveSHA256','pluginSHA256','connectorSHA256')) {
    if ($inputRecipe.$field -cnotmatch '^[a-f0-9]{64}$') { throw "Invalid recipe checksum: $field" }
}
if ((Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $inputRecipe.archiveSHA256) { throw 'AWS CNI archive checksum mismatch' }
if ([Environment]::OSVersion.Version.Build -notin @(20348,26100)) { throw 'Unsupported Windows build' }
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Prepare the protected image directory first' }
if ((Get-Item -LiteralPath $root -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Image directory is a reparse point' }
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Cannot bake an installed worker' }
foreach ($path in @('C:\var\lib\k0s\pki','C:\var\lib\k0s\kubelet.conf','C:\k\config','C:\k0s-token.txt')) {
    if (Test-Path -LiteralPath $path) { throw 'Cannot bake cluster identity' }
}
if (@(Get-ChildItem 'C:\etc\cni\net.d' -File -ErrorAction SilentlyContinue).Count) { throw 'Cannot overwrite existing CNI configuration' }
$kubeconfig='C:\var\lib\k0s\kubelet.conf'
$previous=[Environment]::GetEnvironmentVariable('KUBECONFIG','Machine')
if ($previous -and $previous -ne $kubeconfig) { throw 'Existing machine kubeconfig belongs to another installation' }
$targets=@(
    @{ entry='EKS-Artifacts/vpc-bridge.exe'; path='C:\opt\cni\bin\vpc-bridge.exe'; sha=$inputRecipe.pluginSHA256 },
    @{ entry='EKS-Artifacts/aws-vpc-cni-k8s-connector.exe'; path='C:\Program Files\Amazon\EKS\bin\aws-vpc-cni-k8s-connector.exe'; sha=$inputRecipe.connectorSHA256 }
)
# Check all destinations before changing any of them. Refuse redirected paths
# and a different pre-existing CNI artifact instead of replacing another owner.
foreach ($target in $targets) {
    $path=$target.path
    while ($path) {
        if ((Test-Path -LiteralPath $path) -and ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'AWS CNI destination contains a reparse point' }
        $parent=Split-Path -Parent $path
        if ($parent -eq $path) { break }
        $path=$parent
    }
    if ((Test-Path -LiteralPath $target.path) -and (Get-FileHash -LiteralPath $target.path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $target.sha) { throw 'AWS CNI destination already contains a different artifact' }
}
Add-Type -AssemblyName System.IO.Compression.FileSystem
$staging=Join-Path $root ('aws-cni-stage-'+[Guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $staging
$zip=$null
try {
    $zip=[IO.Compression.ZipFile]::OpenRead((Resolve-Path -LiteralPath $Archive).Path)
    foreach ($target in $targets) {
        $entries=@($zip.Entries | Where-Object FullName -CEQ $target.entry)
        if ($entries.Count -ne 1) { throw 'AWS CNI archive entry missing or ambiguous' }
        $file=Join-Path $staging (Split-Path -Leaf $target.path)
        [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0],$file)
        if ((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -ne $target.sha) { throw 'AWS CNI binary checksum mismatch' }
    }
    foreach ($target in $targets) {
        $null=New-Item -ItemType Directory -Force -Path (Split-Path -Parent $target.path)
        Copy-Item -LiteralPath (Join-Path $staging (Split-Path -Leaf $target.path)) -Destination $target.path -Force
        if ((Get-FileHash -LiteralPath $target.path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $target.sha) { throw 'Installed AWS CNI binary checksum mismatch' }
    }
    # The connector reads KUBECONFIG. Set it before image capture so the fresh
    # worker's service processes inherit it; no cluster credentials are baked.
    [Environment]::SetEnvironmentVariable('KUBECONFIG',$kubeconfig,'Machine')
    $record=@{schemaVersion=1; archiveSHA256=$inputRecipe.archiveSHA256; pluginSHA256=$inputRecipe.pluginSHA256; connectorSHA256=$inputRecipe.connectorSHA256; kubeconfigPath=$kubeconfig; windowsBuild=[Environment]::OSVersion.Version.Build}
    $record | ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $root 'aws-vpc-cni-image.json')
    $record | ConvertTo-Json -Compress
} finally {
    if ($zip) { $zip.Dispose() }
    Remove-Item -LiteralPath $staging -Recurse -Force
}
