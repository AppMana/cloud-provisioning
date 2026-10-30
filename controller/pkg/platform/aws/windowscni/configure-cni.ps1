# Configure AWS's existing plugin on one prepared k0s worker. The plugin owns
# HNS and pod networking; this initializer only creates its configuration file.
param([Parameter(Mandatory=$true)][string]$Settings,[switch]$Reconcile)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$null=& "$PSScriptRoot\verify-cni.ps1" -Recipe "$PSScriptRoot\cni-artifacts.json"
$recipe=Get-Content -LiteralPath "$PSScriptRoot\cni-artifacts.json" -Raw | ConvertFrom-Json
$plan=Get-Content -LiteralPath $Settings -Raw | ConvertFrom-Json
$podGateway='vpc'
if ($plan.PSObject.Properties.Name -contains 'podGateway') { $podGateway=[string]$plan.podGateway }
if ($podGateway -cnotin @('vpc','host')) { throw 'Pod gateway must be vpc or host' }
if ($podGateway -ceq 'host') {
    $owners=@(Get-CimInstance Win32_Service | Where-Object { $_.Name -like 'cloud-provisioning-*' -and $_.State -eq 'Running' -and $_.PathName -match '(?:^|\s)--native-pod-routing(?:=true)?(?:\s|$)' })
    if ($owners.Count -ne 1) { throw 'Host pod gateway requires one running native pod routing service' }
}
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
function PrefixRange($Prefix) {
    $bytes=$Prefix.address.GetAddressBytes()
    $endBytes=[byte[]]::new(4)
    for ($i=0;$i -lt 4;$i++) {
        $remaining=[Math]::Min(8,[Math]::Max(0,$Prefix.bits-$i*8))
        $hostMask=[int][Math]::Pow(2,8-$remaining)-1
        $endBytes[$i]=[byte]($bytes[$i] -bor $hostMask)
    }
    return @{start=$Prefix.address.ToString();end=([Net.IPAddress]::new($endBytes)).ToString();prefix=$Prefix}
}
function MatchesAnyPrefix($Prefixes,[string]$Candidate) {
    $ip=IPv4 $Candidate
    foreach ($prefix in @($Prefixes)) {
        if (Contains $prefix $ip) { return $true }
    }
    return $false
}
function PodMTU($Value,[int]$TunnelMTU) {
    if ($Value -isnot [int] -and $Value -isnot [long]) { throw 'Pod MTU must be an integer' }
    if ($Value -eq 0) { return 0 }
    if ($Value -lt 1280 -or $Value -gt 65535 -or $Value -gt $TunnelMTU) { throw 'Pod MTU exceeds the tunnel budget or is below the dual-stack minimum' }
    return [int]$Value
}
function HostRoutes($Values,$VPCs,$Exclusions,$Service) {
    $result=@()
    foreach ($value in @($Values)) {
        $route=Prefix ([string]$value)
        foreach ($blocked in @('0.0.0.0/8','127.0.0.0/8','169.254.0.0/16','224.0.0.0/3')+@($VPCs)+@($Service.text)) {
            $network=Prefix $blocked
            if ((Contains $network $route.address) -or (Contains $route $network.address)) { throw 'Host route overlaps local, VPC, Service or non-unicast scope' }
        }
        $covered=$false
        foreach ($value in @($Exclusions)) {
            $network=Prefix $value
            if ($network.bits -le $route.bits -and (Contains $network $route.address)) { $covered=$true }
        }
        if (!$covered) { throw 'Host route requires an explicit covering SNAT exclusion' }
        $result+=$route.text
    }
    return @($result | Sort-Object -Unique)
}
$service=Prefix $plan.serviceCIDR
$dns=IPv4 $plan.clusterDNS
if (!(Contains $service $dns)) { throw 'Cluster DNS is outside the Service CIDR' }
$domain=[string]$plan.clusterDomain
if ($domain.Length -gt 253 -or $domain -cnotmatch '^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$') { throw 'Invalid cluster DNS domain' }
$podMTU=0
if ($plan.PSObject.Properties.Name -contains 'podMTU') {
    $podMTU=PodMTU $plan.podMTU 65535
    if ($podMTU) {
        if ($recipe.PSObject.Properties.Name -notcontains 'features' -or 'podMTU' -cnotin @($recipe.features)) { throw 'Prepared CNI recipe does not support pod MTU policies' }
        $owners=@(Get-CimInstance Win32_Service | Where-Object { $_.Name -match '^cloud-provisioning-cldt[0-9a-f]{8}$' -and $_.State -eq 'Running' -and $_.PathName -match '(?:^|\s)--native-pod-routing(?:=true)?(?:\s|$)' })
        if ($owners.Count -ne 1) { throw 'Pod MTU requires one running native pod routing service' }
        $alias=$owners[0].Name.Substring('cloud-provisioning-'.Length)
        $interfaces=@(Get-NetIPInterface -InterfaceAlias $alias -ErrorAction Stop)
        if (!$interfaces.Count) { throw 'Owned tunnel interfaces are unavailable' }
        $podMTU=PodMTU $podMTU ([int]($interfaces | Measure-Object -Property NlMtu -Minimum).Minimum)
    }
}
if (!@($plan.snatExcludedCIDRs).Count) { throw 'Explicit site SNAT exclusions are required' }
$exclusions=@($plan.snatExcludedCIDRs | ForEach-Object { (Prefix $_).text })
$defRoute=Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Select-Object -First 1
if ($defRoute -and !(Get-NetRoute -InterfaceIndex $defRoute.InterfaceIndex -DestinationPrefix '169.254.169.254/32' -ErrorAction SilentlyContinue)) {
    foreach ($imds in @('169.254.169.254/32','169.254.169.250/32','169.254.169.251/32','169.254.169.253/32','169.254.169.123/32')) {
        Remove-NetRoute -DestinationPrefix $imds -Confirm:$false -ErrorAction SilentlyContinue
        $null=New-NetRoute -InterfaceIndex $defRoute.InterfaceIndex -DestinationPrefix $imds -NextHop $defRoute.NextHop -RouteMetric 1 -PolicyStore ActiveStore -ErrorAction SilentlyContinue
    }
}
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
$hostRoutes=@()
if ($plan.PSObject.Properties.Name -contains 'hostRouteCIDRs' -and @($plan.hostRouteCIDRs).Count) {
    if ($podGateway -cne 'vpc') { throw 'Host route policies require the VPC pod gateway' }
    if ($recipe.PSObject.Properties.Name -notcontains 'features' -or 'hostRouteCIDRs' -cnotin @($recipe.features)) { throw 'Prepared CNI recipe does not support host route policies' }
    $hostRoutes=@(HostRoutes $plan.hostRouteCIDRs $vpc $exclusions $service)
    $owners=@(Get-CimInstance Win32_Service | Where-Object { $_.Name -like 'cloud-provisioning-*' -and $_.State -eq 'Running' -and $_.PathName -match '(?:^|\s)--native-pod-routing(?:=true)?(?:\s|$)' })
    if ($owners.Count -ne 1) { throw 'Host route policies require one running native pod routing service' }
}
$adapters=@(Get-NetAdapter -Physical | Where-Object { ($_.MacAddress -replace '-',':') -ieq $mac })
if ($adapters.Count -ne 1) { throw 'Primary ENI does not identify exactly one physical adapter' }
$index=$adapters[0].ifIndex
$vnic=@(Get-NetAdapter -IncludeHidden | Where-Object { $_.Name -like 'vEthernet (*' -and (($_.MacAddress -replace '-',':') -ieq $mac) })
if ($vnic.Count -eq 1 -and @(Get-NetIPAddress -InterfaceIndex $vnic[0].ifIndex -AddressFamily IPv4 | Where-Object IPAddress -EQ $ip.ToString()).Count -eq 1) { $index=$vnic[0].ifIndex }
if (@(Get-NetIPAddress -InterfaceIndex $index -AddressFamily IPv4 | Where-Object IPAddress -EQ $ip.ToString()).Count -ne 1) { throw 'Primary ENI address is not configured on its adapter' }
$gateways=@(Get-NetRoute -InterfaceIndex $index -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Select-Object -ExpandProperty NextHop -Unique)
if ($gateways.Count -ne 1 -or !(Contains $subnet (IPv4 $gateways[0]))) { throw 'Primary ENI gateway is missing or ambiguous' }
$podGatewayAddress=if ($podGateway -ceq 'host') { $ip.ToString() } else { $gateways[0] }
$config=[ordered]@{
    cniVersion='0.4.0';name='vpc';type='vpc-bridge';capabilities=[ordered]@{portMappings=$true};disableCheck=$true
    eniMACAddress=$mac.ToLowerInvariant();eniIPAddresses=@($ip.ToString()+'/'+$subnet.bits)
    gatewayIPAddress=$podGatewayAddress;vpcCIDRs=@(@($vpc)+@($exclusions)+@($service.text) | Sort-Object -Unique)
    serviceCIDR=$service.text;dns=[ordered]@{nameservers=@($dns.ToString());search=@(('{%namespace%}.svc.'+$domain),('svc.'+$domain),$domain)}
}
if ($hostRoutes.Count) { $config.hostRouteCIDRs=$hostRoutes }
if ($podMTU) { $config.podMTU=$podMTU }
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
if ($Reconcile) {
    $macClean=($mac -replace '[:-]','').ToLowerInvariant()
    $macDash=($mac -replace ':','-').ToUpperInvariant()
    $paIP=$ip.ToString()
    $netName="vpcbr$macClean"
    $hostRoutePrefixes=@($hostRoutes | ForEach-Object { Prefix $_ })
    $hostRouteRanges=@($hostRoutePrefixes | ForEach-Object { PrefixRange $_ })
    $snatIP=$null
    try {
        $pub=Metadata 'public-ipv4'
        if ($pub) {
            $parsedPub=IPv4 $pub
            if ($parsedPub.ToString() -cne $paIP) { $snatIP=$parsedPub.ToString() }
        }
    } catch {}
    $vfp='C:\Windows\System32\vfpctrl.exe'
    $mapSpace='C319F3C6-7533-4658-993A-8415288DCF04'
    while ($true) {
        try {
            $def=Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Select-Object -First 1
            if ($def) {
                $ifIdx=$def.InterfaceIndex
                if (!(Get-NetRoute -InterfaceIndex $ifIdx -DestinationPrefix '169.254.169.254/32' -ErrorAction SilentlyContinue)) {
                    foreach ($imds in @('169.254.169.254/32','169.254.169.250/32','169.254.169.251/32','169.254.169.253/32','169.254.169.123/32')) {
                        Remove-NetRoute -DestinationPrefix $imds -Confirm:$false -ErrorAction SilentlyContinue
                        $null=New-NetRoute -InterfaceIndex $ifIdx -DestinationPrefix $imds -NextHop $def.NextHop -RouteMetric 1 -PolicyStore ActiveStore -ErrorAction SilentlyContinue
                    }
                    Start-Service AmazonSSMAgent -ErrorAction SilentlyContinue
                }
                Set-NetIPInterface -InterfaceIndex $ifIdx -AddressFamily IPv4 -Forwarding Enabled -WeakHostSend Enabled -WeakHostReceive Enabled -ErrorAction SilentlyContinue
                foreach ($wg in @(Get-NetAdapter -ErrorAction SilentlyContinue | Where-Object Name -Like 'cldt*')) {
                    Set-NetIPInterface -InterfaceIndex $wg.ifIndex -AddressFamily IPv4 -Forwarding Enabled -WeakHostSend Enabled -WeakHostReceive Enabled -ErrorAction SilentlyContinue
                }
                if ($snatIP) {
                    if (!(Get-NetRoute -InterfaceIndex $ifIdx -DestinationPrefix "$snatIP/32" -ErrorAction SilentlyContinue)) {
                        $null=& 'C:\Windows\System32\route.exe' add $snatIP mask 255.255.255.255 0.0.0.0 if $ifIdx metric 5 2>$null
                    }
                    if (!(Get-NetNeighbor -InterfaceIndex $ifIdx -IPAddress $snatIP -ErrorAction SilentlyContinue)) {
                        $null=New-NetNeighbor -InterfaceIndex $ifIdx -IPAddress $snatIP -LinkLayerAddress $macDash -State Permanent -ErrorAction SilentlyContinue
                    }
                }
            }
            $hnsNet=Get-HnsNetwork -ErrorAction SilentlyContinue | Where-Object Name -EQ $netName
            if ($hnsNet) {
                $kpBin='C:\var\lib\k0s\bin\kube-proxy.exe'
                $kpCfg='C:\var\lib\k0s\kubelet-bootstrap.conf'
                if ((Test-Path -LiteralPath $kpBin) -and (Test-Path -LiteralPath $kpCfg)) {
                    $kpProc=@(Get-CimInstance Win32_Process -Filter "Name = 'kube-proxy.exe'" -ErrorAction SilentlyContinue)
                    foreach ($proc in $kpProc) {
                        if ($proc.CommandLine -notmatch "--network-name=$([Regex]::Escape($netName))") {
                            Stop-Process -Id $proc.ProcessId -Force -ErrorAction SilentlyContinue
                        }
                    }
                    if (!(Get-Process kube-proxy -ErrorAction SilentlyContinue)) {
                        $clusterCIDR=if ($hostRoutes.Count) { $hostRoutes[0] } else { $service.text }
                        $null=Start-Process -FilePath $kpBin -ArgumentList @('--v=4','--proxy-mode=kernelspace',"--hostname-override=$($env:COMPUTERNAME.ToLowerInvariant())","--kubeconfig=$kpCfg","--network-name=$netName","--cluster-cidr=$clusterCIDR") -WindowStyle Hidden
                    }
                }
                if ((Test-Path -LiteralPath $vfp) -and $hostRouteRanges.Count) {
                    $ports=& $vfp /list-vmswitch-port | Select-String 'Port name\s*:\s*([0-9A-Fa-f-]+)' | ForEach-Object { $_.Matches[0].Groups[1].Value }
                    foreach ($p in @($ports)) {
                        $paRules=(& $vfp /list-rule /port $p /layer VNET_PA_ROUTE_LAYER) -join "`n"
                        if ($paRules -notmatch 'VNET_GROUP_PA_ROUTE_IPV4_OUT') { continue }
                        $maps=(& $vfp /list-mapping /port $p) -join "`n"
                        $needsMappingFix=$false
                        $blocks=$maps -split 'MAPPING ID\s*:'
                        foreach ($b in $blocks) {
                            if ($b -match [Regex]::Escape($mapSpace) -and $b -match 'CA IP\s*:\s*([0-9.]+)' -and $b -match 'Customer MAC address\s*:\s*02-11-') {
                                if (MatchesAnyPrefix $hostRoutePrefixes $Matches[1]) { $needsMappingFix=$true; break }
                            }
                        }
                        if ($paRules -notmatch 'PA_ALLOW_SITE_0' -or $needsMappingFix) {
                            foreach ($b in $blocks) {
                                if ($b -match [Regex]::Escape($mapSpace) -and $b -match 'CA IP\s*:\s*([0-9.]+)') {
                                    $caIP=$Matches[1]
                                    if (MatchesAnyPrefix $hostRoutePrefixes $caIP) {
                                        $null=& $vfp /update-mapping /port $p """$mapSpace $caIP $paIP $macDash"""
                                    }
                                }
                            }
                            if ($snatIP) {
                                $null=& $vfp /deposit-nat-pool /port $p """SITE_NATPOOL_TCP 6 $snatIP-$snatIP 1024-65535"""
                                $null=& $vfp /deposit-nat-pool /port $p """SITE_NATPOOL_UDP 17 $snatIP-$snatIP 1024-65535"""
                                $lbRules=((& $vfp /list-rule /port $p /layer SLB_LB_LAYER) -join "`n") -split 'RULE\s*:'
                                $siteVips=@{}
                                foreach ($lb in $lbRules) {
                                    if ($lb -match 'LB_DSR_OUT_IPV4_([0-9.]+)_[0-9]+_[0-9]+') {
                                        $vip=$Matches[1]
                                        $dips=[Regex]::Matches($lb,'DIP:\s*([0-9.]+)')
                                        foreach ($d in $dips) {
                                            if (MatchesAnyPrefix $hostRoutePrefixes $d.Groups[1].Value) { $siteVips[$vip]=$true; break }
                                        }
                                    }
                                }
                                $natRules=(& $vfp /list-rule /port $p /layer SLB_NAT_LAYER) -join "`n"
                                foreach ($vip in $siteVips.Keys) {
                                    $ev=[Regex]::Escape($vip)
                                    $m=[Regex]::Matches($natRules,"(SNAT_(TCP|UDP)_${ev}_(\d+))\s+Friendly name\s*:\s*\1\s+Priority\s*:\s*(\d+)[\s\S]*?Protocol\s*:\s*(\d+)")
                                    foreach ($match in $m) {
                                        $rName=$match.Groups[1].Value
                                        $protoName=$match.Groups[2].Value
                                        $dPort=$match.Groups[3].Value
                                        $prio=$match.Groups[4].Value
                                        $protoNum=$match.Groups[5].Value
                                        $poolName="SITE_NATPOOL_$protoName"
                                        $null=& $vfp /add-rule-ex /port $p /layer SLB_NAT_LAYER /group SLB_GROUP_NAT_IPV4_OUT """$rName $rName $prio $protoNum * $vip * $dPort snat 0x0 $poolName"""
                                    }
                                    $vipRule="PA_ALLOW_SITE_VIP_$($vip -replace '\.','_')"
                                    $null=& $vfp /add-rule-ex /port $p /layer VNET_PA_ROUTE_LAYER /group VNET_GROUP_PA_ROUTE_IPV4_OUT """$vipRule $vipRule 109 * * $vip-$vip * * allow 0x0"""
                                }
                            }
                            for ($i=0;$i -lt $hostRouteRanges.Count;$i++) {
                                $r=$hostRouteRanges[$i]
                                $prio=110+$i
                                $null=& $vfp /add-rule-ex /port $p /layer VNET_PA_ROUTE_LAYER /group VNET_GROUP_PA_ROUTE_IPV4_OUT """PA_ALLOW_SITE_$i PA_ALLOW_SITE_$i $prio * * $($r.start)-$($r.end) * * allow 0x0"""
                            }
                        }
                    }
                }
            }
        } catch {}
        Start-Sleep -Seconds 1
    }
}
