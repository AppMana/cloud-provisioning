# Native image-layer regression test. Requires a fresh, unjoined image builder.
param(
    [Parameter(Mandatory=$true)][string]$Archive,
    [string]$Recipe=$(if (Test-Path (Join-Path $PSScriptRoot 'cni-artifacts.json')) { Join-Path $PSScriptRoot 'cni-artifacts.json' } else { Join-Path $PSScriptRoot '..\..\..\controller\pkg\platform\aws\windowscni\cni-artifacts.json' })
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$verifyScript=if (Test-Path (Join-Path $PSScriptRoot 'verify-cni.ps1')) { Join-Path $PSScriptRoot 'verify-cni.ps1' } else { Join-Path $PSScriptRoot '..\..\..\controller\pkg\platform\aws\windowscni\verify-cni.ps1' }
$root='C:\ProgramData\CloudProvisioning'
$plugin='C:\opt\cni\bin\vpc-bridge.exe'
$connector='C:\Program Files\Amazon\EKS\bin\aws-vpc-cni-k8s-connector.exe'
$receipt=Join-Path $root 'aws-vpc-cni-image.json'
foreach ($path in @($plugin,$connector,$receipt)) {
    if (Test-Path -LiteralPath $path) { throw 'Test requires an image without the AWS CNI layer' }
}
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Test requires an unjoined builder' }
$beforeRoutes=Get-NetRoute -AddressFamily IPv4 | Select-Object InterfaceIndex,DestinationPrefix,NextHop,RouteMetric | Sort-Object InterfaceIndex,DestinationPrefix,NextHop | ConvertTo-Json -Compress
$tests=[Collections.Generic.List[string]]::new()
$temp=Join-Path $root ('cni-test-'+[Guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $temp
function Expect-Failure([scriptblock]$Action,[string]$Message) {
    $caught=$false
    try { $null=& $Action } catch {
        if (!$_.Exception.Message.Contains($Message)) { throw }
        $caught=$true
    }
    if (!$caught) { throw "Expected failure: $Message" }
}
try {
    foreach ($field in @('archiveSHA256','pluginSHA256','connectorSHA256')) {
        $bad=Get-Content -LiteralPath $Recipe -Raw | ConvertFrom-Json
        $bad.$field='0'*64
        $badPath=Join-Path $temp ($field+'.json')
        $bad | ConvertTo-Json | Set-Content -Encoding UTF8 $badPath
        $message=if ($field -eq 'archiveSHA256') { 'AWS CNI archive checksum mismatch' } else { 'AWS CNI binary checksum mismatch' }
        Expect-Failure { & "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $badPath } $message
        foreach ($path in @($plugin,$connector,$receipt)) {
            if (Test-Path -LiteralPath $path) { throw 'Rejected recipe changed an installation destination' }
        }
        if (@(Get-ChildItem $root -Filter 'aws-cni-stage-*').Count) { throw 'Rejected recipe left extraction state' }
        $tests.Add($field+' rejected without installation')
    }
    $first=& "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $Recipe | ConvertFrom-Json
    $verified=& $verifyScript -Recipe $Recipe | ConvertFrom-Json
    if ($first.pluginSHA256 -ne $verified.pluginSHA256 -or $first.connectorSHA256 -ne $verified.connectorSHA256) { throw 'Installed verification differs from preparation' }
    $tests.Add('native installation and readback')
    $repeat=& "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $Recipe | ConvertFrom-Json
    if (($repeat | ConvertTo-Json -Compress) -ne ($first | ConvertTo-Json -Compress)) { throw 'Repeated preparation changed identity' }
    $tests.Add('idempotent preparation')
    $backup=Join-Path $temp 'plugin.exe'
    Copy-Item -LiteralPath $plugin -Destination $backup
    try {
        [IO.File]::WriteAllText($plugin,'invalid executable')
        Expect-Failure { & $verifyScript -Recipe $Recipe } 'Installed AWS CNI checksum mismatch'
        Expect-Failure { & "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $Recipe } 'AWS CNI destination already contains a different artifact'
        if ([IO.File]::ReadAllText($plugin) -ne 'invalid executable') { throw 'Preparation overwrote an unexpected binary' }
    } finally { Copy-Item -LiteralPath $backup -Destination $plugin -Force }
    $tests.Add('tamper detected and unexpected artifact preserved')
    $null=& $verifyScript -Recipe $Recipe
    if (@(Get-ChildItem 'C:\etc\cni\net.d' -File -ErrorAction SilentlyContinue).Count) { throw 'Image preparation created node-specific CNI configuration' }
    if (Test-Path 'C:\var\lib\k0s\kubelet.conf') { throw 'Image preparation created cluster credentials' }
    if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Image preparation installed a worker service' }
    $afterRoutes=Get-NetRoute -AddressFamily IPv4 | Select-Object InterfaceIndex,DestinationPrefix,NextHop,RouteMetric | Sort-Object InterfaceIndex,DestinationPrefix,NextHop | ConvertTo-Json -Compress
    if ($beforeRoutes -ne $afterRoutes) { throw 'Routing changed during image preparation' }
    $tests.Add('no routes, cluster identity, worker service or CNI configuration created')
    @{windowsBuild=[Environment]::OSVersion.Version.Build;passed=$true;tests=@($tests);installation=$verified} | ConvertTo-Json -Depth 5 -Compress
} finally { Remove-Item -LiteralPath $temp -Recurse -Force }
