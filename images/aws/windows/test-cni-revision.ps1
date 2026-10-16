# Verify revision guards on a prepared, unjoined image builder.
param(
    [Parameter(Mandatory=$true)][string]$Archive,
    [Parameter(Mandatory=$true)][string]$Recipe,
    [Parameter(Mandatory=$true)][string]$PreviousRecipe
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Test requires an unjoined builder' }
$root='C:\ProgramData\CloudProvisioning'
$plugin='C:\opt\cni\bin\vpc-bridge.exe'
$receipt=Join-Path $root 'aws-vpc-cni-image.json'
$initial=& "$PSScriptRoot\verify-cni.ps1" -Recipe $Recipe | ConvertFrom-Json
$beforeRoutes=Get-NetRoute -AddressFamily IPv4 | Select-Object InterfaceIndex,DestinationPrefix,NextHop,RouteMetric | Sort-Object InterfaceIndex,DestinationPrefix,NextHop | ConvertTo-Json -Compress
$tests=[Collections.Generic.List[string]]::new()
$temp=Join-Path $root ('cni-revision-test-'+[Guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $temp
try {
    $repeat=& "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $Recipe -PreviousRecipe $PreviousRecipe | ConvertFrom-Json
    foreach ($field in @('schemaVersion','archiveSHA256','pluginSHA256','connectorSHA256','kubeconfigPath','windowsBuild')) {
        if ($repeat.$field -cne $initial.$field) { throw 'Repeated revision changed identity' }
    }
    $tests.Add('completed revision remains idempotent with prior recipe')
    $recordBefore=[IO.File]::ReadAllText($receipt)
    $backup=Join-Path $temp 'plugin.exe'
    Copy-Item -LiteralPath $plugin -Destination $backup
    try {
        [IO.File]::WriteAllText($plugin,'unexpected artifact')
        $rejected=$false
        try { $null=& "$PSScriptRoot\prepare-cni.ps1" -Archive $Archive -Recipe $Recipe -PreviousRecipe $PreviousRecipe } catch { $rejected=$true }
        if (!$rejected) { throw 'Revision accepted an unverified existing installation' }
        if ([IO.File]::ReadAllText($plugin) -cne 'unexpected artifact' -or [IO.File]::ReadAllText($receipt) -cne $recordBefore) { throw 'Rejected revision changed existing artifacts or receipt' }
    } finally { Copy-Item -LiteralPath $backup -Destination $plugin -Force }
    $tests.Add('tampered installation rejected without replacement')
    $verified=& "$PSScriptRoot\verify-cni.ps1" -Recipe $Recipe | ConvertFrom-Json
    $afterRoutes=Get-NetRoute -AddressFamily IPv4 | Select-Object InterfaceIndex,DestinationPrefix,NextHop,RouteMetric | Sort-Object InterfaceIndex,DestinationPrefix,NextHop | ConvertTo-Json -Compress
    if ($beforeRoutes -cne $afterRoutes) { throw 'Image revision changed routing' }
    if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Image revision installed a worker' }
    if (Test-Path 'C:\var\lib\k0s\kubelet.conf') { throw 'Image revision created cluster credentials' }
    $tests.Add('restored candidate verified without runtime or network changes')
    @{windowsBuild=[Environment]::OSVersion.Version.Build;passed=$true;tests=@($tests);installation=$verified} | ConvertTo-Json -Depth 5 -Compress
} finally { Remove-Item -LiteralPath $temp -Recurse -Force }
