# Configure AWS's existing plugin on one prepared k0s worker. The plugin owns
# HNS and pod networking; this initializer only creates its configuration file.
param([Parameter(Mandatory=$true)][string]$Settings)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$null=& "$PSScriptRoot\verify-cni.ps1" -Recipe "$PSScriptRoot\cni-artifacts.json"
$plan=Get-Content -LiteralPath $Settings -Raw | ConvertFrom-Json
function IPv4([string]$Value) {
    if ($Value -notmatch '^([0-9]{1,3}\.){3}[0-9]{1,3}$') { throw 'Expected an IPv4 address' }
    $ip=[Net.IPAddress]::Parse($Value)
    if ($ip.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { throw 'Expected an IPv4 address' }
    return $ip
}
function Prefix([string]$Value) {
    $parts=$Value.Split('/')
    if ($parts.Count -ne 2 -or $parts[1] -notmatch '^(?:[1-9]|[12][0-9]|3[0-2])$') { throw 'Expected an explicit IPv4 prefix' }
    $ip=IPv4 $parts[0]
    $bits=[int]$parts[1]
    $bytes=$ip.GetAddressBytes()
    for ($i=0;$i -lt 4;$i++) {
        $remaining=[Math]::Min(8,[Math]::Max(0,$bits-$i*8))
        $mask=256-[int][Math]::Pow(2,8-$remaining)
        if (($bytes[$i] -band $mask) -ne $bytes[$i]) { throw 'IPv4 prefix has host bits set' }
    }
    return @{address=$ip;bits=$bits;text=$ip.ToString()+'/'+$bits}
}
function Contains($Prefix,[Net.IPAddress]$Address) {
    $network=$Prefix.address.GetAddressBytes();$bytes=$Address.GetAddressBytes()
    for ($i=0;$i -lt 4;$i++) {
        $remaining=[Math]::Min(8,[Math]::Max(0,$Prefix.bits-$i*8))
        $mask=256-[int][Math]::Pow(2,8-$remaining)
        if (($bytes[$i] -band $mask) -ne $network[$i]) { return $false }
    }
    return $true
}
$service=Prefix $plan.serviceCIDR
$dns=IPv4 $plan.clusterDNS
if (!(Contains $service $dns)) { throw 'Cluster DNS is outside the Service CIDR' }
$domain=[string]$plan.clusterDomain
if ($domain.Length -gt 253 -or $domain -cnotmatch '^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$') { throw 'Invalid cluster DNS domain' }
if (!@($plan.snatExcludedCIDRs).Count) { throw 'Explicit site SNAT exclusions are required' }
$exclusions=@($plan.snatExcludedCIDRs | ForEach-Object { (Prefix $_).text })
$base='http://169.254.169.254/latest'
$token=Invoke-RestMethod -Method Put -Uri "$base/api/token" -Headers @{'X-aws-ec2-metadata-token-ttl-seconds'='60'} -TimeoutSec 5
$headers=@{'X-aws-ec2-metadata-token'=$token}
function Metadata([string]$Path) {
    return (Invoke-RestMethod -Uri "$base/meta-data/$Path" -Headers $headers -TimeoutSec 5).Trim()
}
$mac=Metadata 'mac'
if ($mac -notmatch '^(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$') { throw 'Invalid primary ENI MAC' }
$ip=IPv4 (Metadata 'local-ipv4')
$subnet=Prefix (Metadata "network/interfaces/macs/$mac/subnet-ipv4-cidr-block")
if (!(Contains $subnet $ip)) { throw 'Primary ENI address is outside its subnet' }
$vpc=@((Metadata "network/interfaces/macs/$mac/vpc-ipv4-cidr-blocks") -split '\s+' | ForEach-Object { (Prefix $_).text })
foreach ($cidr in $vpc) {
    $network=Prefix $cidr
    if ((Contains $network $service.address) -or (Contains $service $network.address)) { throw 'VPC and Service CIDRs overlap' }
}
$adapters=@(Get-NetAdapter -Physical | Where-Object { ($_.MacAddress -replace '-',':') -ieq $mac })
if ($adapters.Count -ne 1) { throw 'Primary ENI does not identify exactly one physical adapter' }
$index=$adapters[0].ifIndex
if (@(Get-NetIPAddress -InterfaceIndex $index -AddressFamily IPv4 | Where-Object IPAddress -EQ $ip.ToString()).Count -ne 1) { throw 'Primary ENI address is not configured on its adapter' }
$gateways=@(Get-NetRoute -InterfaceIndex $index -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Select-Object -ExpandProperty NextHop -Unique)
if ($gateways.Count -ne 1 -or !(Contains $subnet (IPv4 $gateways[0]))) { throw 'Primary ENI gateway is missing or ambiguous' }
$config=[ordered]@{
    cniVersion='0.4.0';name='vpc';type='vpc-bridge';capabilities=[ordered]@{portMappings=$true};disableCheck=$true
    eniMACAddress=$mac.ToLowerInvariant();eniIPAddresses=@($ip.ToString()+'/'+$subnet.bits)
    gatewayIPAddress=$gateways[0];vpcCIDRs=@(@($vpc)+@($exclusions)+@($service.text) | Sort-Object -Unique)
    serviceCIDR=$service.text;dns=[ordered]@{nameservers=@($dns.ToString());search=@(('{%namespace%}.svc.'+$domain),('svc.'+$domain),$domain)}
}
$json=$config | ConvertTo-Json -Depth 5
$directory='C:\etc\cni\net.d'
$destination=Join-Path $directory 'vpc-bridge.conf'
$path=$destination
while ($path) {
    if ((Test-Path -LiteralPath $path) -and ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'CNI configuration path contains a reparse point' }
    $parent=Split-Path -Parent $path
    if ($parent -eq $path) { break }
    $path=$parent
}
if (@(Get-ChildItem -LiteralPath $directory -File -ErrorAction SilentlyContinue | Where-Object Name -NE 'vpc-bridge.conf').Count) { throw 'Existing CNI configuration belongs to another installation' }
if (Test-Path -LiteralPath $destination) {
    if ([IO.File]::ReadAllText($destination) -cne $json) { throw 'Existing AWS CNI configuration differs; refusing a live network replacement' }
} else {
    $null=New-Item -ItemType Directory -Force -Path $directory
    $temp=Join-Path $directory ([Guid]::NewGuid().ToString('N')+'.tmp')
    try {
        [IO.File]::WriteAllText($temp,$json,[Text.UTF8Encoding]::new($false))
        [IO.File]::Move($temp,$destination)
    } finally { if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Force } }
}
@{configured=$true;path=$destination;primaryIP=$ip.ToString();mac=$mac;serviceCIDR=$service.text} | ConvertTo-Json -Compress
